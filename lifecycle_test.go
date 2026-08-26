package dynamicconfig_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

// barrier is a validator that lets a test stop a reload in the middle of
// the transaction — after the file has been read and decoded, before
// anything has been published.
//
// It is how the close/publish ordering is tested without a single
// time.Sleep: the reload is held at a known point, the competing operation
// is run to completion, and only then is the reload released.
type barrier struct {
	entered chan struct{}
	release chan struct{}

	armed atomic.Bool
}

func newBarrier() *barrier {
	return &barrier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// validator blocks exactly once, on the first call made after arm.
func (b *barrier) validator(_ *appConfig) error {
	if b.armed.CompareAndSwap(true, false) {
		close(b.entered)

		<-b.release
	}

	return nil
}

func (b *barrier) arm()     { b.armed.Store(true) }
func (b *barrier) unblock() { close(b.release) }

func TestClosePreventsInFlightReloadFromPublishing(t *testing.T) {
	t.Parallel()

	held := newBarrier()

	path := newConfigFile(t, baseConfig)

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(held.validator),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	published := cfg.Current()
	generation := cfg.Generation()

	writeConfig(t, path, "server:\n  host: never-published\n  port: 9000\n")

	held.arm()

	reloaded := make(chan error, 1)

	go func() { reloaded <- cfg.Reload(context.Background()) }()

	// The reload is now inside the transaction, past read and decode,
	// holding a fully valid candidate it has not published.
	<-held.entered

	closed := make(chan error, 1)

	go func() { closed <- cfg.Close() }()

	waitFor(t, func() bool { return cfg.Status().Closed }, "the close transition to win")

	held.unblock()

	if err := <-reloaded; !errors.Is(err, dynamicconfig.ErrClosed) {
		t.Fatalf("Reload = %v, want ErrClosed: a closed config must not publish", err)
	}

	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}

	if cfg.Current() != published {
		t.Fatal("a reload published a snapshot after Close had begun")
	}

	if got := cfg.Generation(); got != generation {
		t.Fatalf("generation = %d, want %d frozen at close", got, generation)
	}

	// The rejected commit is a shutdown, not a verdict on the
	// configuration, so it is not counted as a failed reload.
	if got := cfg.Status().FailedReloads; got != 0 {
		t.Fatalf("failed reloads = %d, want 0: closing is not a rejection", got)
	}
}

func TestReloadCanPublishBeforeCloseWinsCommitGate(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	writeConfig(t, path, "server:\n  host: published\n  port: 9000\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	published := cfg.Current()

	if published.Server.Host != "published" || cfg.Generation() != 2 {
		t.Fatalf("the reload did not publish: %+v generation %d", published.Server, cfg.Generation())
	}

	// The other side of the total order: a commit that won stays won, and
	// closing freezes it rather than undoing it.
	if err := cfg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if cfg.Current() != published {
		t.Fatal("Close disturbed the snapshot a reload had already published")
	}

	if got := cfg.Generation(); got != 2 {
		t.Fatalf("generation = %d, want 2 frozen at close", got)
	}
}

func TestReloadWaitingForSemaphoreReturnsClosed(t *testing.T) {
	t.Parallel()

	held := newBarrier()

	path := newConfigFile(t, baseConfig)

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(held.validator),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	held.arm()

	first := make(chan error, 1)

	go func() { first <- cfg.Reload(context.Background()) }()

	<-held.entered

	// A second reload can only queue behind the first, on a wait that has
	// to be escapable — otherwise Close would be blocked by a reload that
	// is itself blocked.
	second := make(chan error, 1)

	go func() { second <- cfg.Reload(context.Background()) }()

	closed := make(chan error, 1)

	go func() { closed <- cfg.Close() }()

	if err := <-second; !errors.Is(err, dynamicconfig.ErrClosed) {
		t.Fatalf("queued Reload = %v, want ErrClosed", err)
	}

	held.unblock()

	<-first
	<-closed
}

func TestConcurrentCloseAndReloadManyTimes(t *testing.T) {
	t.Parallel()

	for round := range 50 {
		cfg, path := newTestConfig(t)

		writeConfig(t, path, fmt.Sprintf("server:\n  host: round\n  port: %d\n", 9000+round))

		var wg sync.WaitGroup

		for range 4 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				_ = cfg.Reload(context.Background())
			}()
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = cfg.Close()
		}()

		wg.Wait()

		// Whatever order the race resolved in, the state afterwards has
		// to be one of the legal ones: a snapshot exists, and nothing
		// moves once the config is closed.
		if cfg.Current() == nil {
			t.Fatalf("round %d: the snapshot was cleared", round)
		}

		frozen := cfg.Generation()
		snapshot := cfg.Current()

		if err := cfg.Reload(context.Background()); !errors.Is(err, dynamicconfig.ErrClosed) {
			t.Fatalf("round %d: Reload after Close = %v", round, err)
		}

		if cfg.Generation() != frozen || cfg.Current() != snapshot {
			t.Fatalf("round %d: state moved after close", round)
		}
	}
}

func TestValidatorPanicRejectsCandidate(t *testing.T) {
	t.Parallel()

	var explode atomic.Bool

	path := newConfigFile(t, baseConfig)

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(func(c *appConfig) error {
			if explode.Load() {
				panic("validator exploded")
			}

			return validPort(c)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	good := cfg.Current()

	failures := make(chan dynamicconfig.ReloadError, 4)

	cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) { failures <- e })

	explode.Store(true)

	writeConfig(t, path, "server:\n  host: rejected\n  port: 9000\n")

	err = cfg.Reload(t.Context())
	if err == nil {
		t.Fatal("a panicking validator accepted the candidate")
	}

	// A validator is application code on a recoverable path. A panic in
	// one is a rejected candidate, not a dead process.
	if cfg.Current() != good {
		t.Fatal("a panicking validator disturbed the published snapshot")
	}

	select {
	case e := <-failures:
		if e.Stage != dynamicconfig.StageValidation {
			t.Fatalf("stage = %q, want %q", e.Stage, dynamicconfig.StageValidation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the validator panic was not reported")
	}

	// And the machinery survives it.
	explode.Store(false)

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload after a validator panic: %v", err)
	}

	if cfg.Current().Server.Host != "rejected" {
		t.Fatal("the configuration stopped reloading after a validator panic")
	}
}

func TestValidatorPanicDuringInitialLoadReturnsError(t *testing.T) {
	t.Parallel()

	path := newConfigFile(t, baseConfig)

	_, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(func(*appConfig) error {
			panic("validator exploded during startup")
		}),
	)
	if err == nil {
		t.Fatal("New succeeded with a panicking validator")
	}

	if got := err.Error(); !contains(got, "panicked") {
		t.Fatalf("error does not mention the panic: %v", err)
	}
}

func TestSealedConfigHidesTheEngine(t *testing.T) {
	t.Parallel()

	path := newConfigFile(t, baseConfig)

	sealed, err := dynamicconfig.NewSealed[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		t.Fatalf("NewSealed: %v", err)
	}

	defer func() { _ = sealed.Close() }()

	if !sealed.Sealed() {
		t.Fatal("NewSealed produced an unsealed configuration")
	}

	if sealed.Viper() != nil {
		t.Fatal("a sealed configuration handed back the engine")
	}

	// Sealing hides the engine; it changes nothing else.
	if sealed.Current().Server.Port != 8080 {
		t.Fatalf("unexpected snapshot: %+v", sealed.Current().Server)
	}

	writeConfig(t, path, "server:\n  host: sealed\n  port: 9000\n")

	if err := sealed.Reload(t.Context()); err != nil {
		t.Fatalf("Reload on a sealed configuration: %v", err)
	}

	if sealed.Current().Server.Host != "sealed" {
		t.Fatal("a sealed configuration did not reload")
	}

	if got := sealed.Status().ConfigFile; got != path {
		t.Fatalf("status config file = %q, want %q", got, path)
	}
}

func TestOpenConfigExposesTheEngine(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	if cfg.Sealed() {
		t.Fatal("New produced a sealed configuration")
	}

	v := cfg.Viper()
	if v == nil {
		t.Fatal("an open configuration hid the engine")
	}

	if got := v.GetInt("server.port"); got != 8080 {
		t.Fatalf("viper port = %d", got)
	}
}

// TestNoGoroutinesLeak walks the lifecycles that create goroutines and
// checks that each of them gives them back.
//
// Deliberately not parallel: runtime.NumGoroutine is a property of the
// process, so a sibling test running alongside would be counted too.
func TestNoGoroutinesLeak(t *testing.T) {
	scenarios := map[string]func(t *testing.T, path string){
		"new then close": func(t *testing.T, path string) {
			cfg := mustOpen(t, path)

			if err := cfg.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		},

		"watch cancelled then closed": func(t *testing.T, path string) {
			cfg := mustOpen(t, path)

			ctx, cancel := context.WithCancel(context.Background())

			stopped := make(chan error, 1)

			go func() { stopped <- cfg.Watch(ctx) }()

			waitFor(t, func() bool { return cfg.Status().Watching }, "the watcher to start")

			cancel()
			<-stopped

			_ = cfg.Close()
		},

		"watch stopped by close alone": func(t *testing.T, path string) {
			cfg := mustOpen(t, path)

			stopped := make(chan error, 1)

			go func() { stopped <- cfg.Watch(context.Background()) }()

			waitFor(t, func() bool { return cfg.Status().Watching }, "the watcher to start")

			_ = cfg.Close()

			<-stopped
		},

		"subscriber panicked then closed": func(t *testing.T, path string) {
			cfg := mustOpen(t, path)

			cfg.Subscribe(func(dynamicconfig.Change[appConfig]) { panic("boom") })

			writeConfig(t, path, "server:\n  host: panic\n  port: 9001\n")

			_ = cfg.Reload(context.Background())

			_ = cfg.Close()
		},

		"reload storm then close": func(t *testing.T, path string) {
			cfg := mustOpen(t, path)

			for i := range 50 {
				writeConfig(t, path, fmt.Sprintf("server:\n  host: storm\n  port: %d\n", 9000+i))

				_ = cfg.Reload(context.Background())
			}

			_ = cfg.Close()
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			before := runtime.NumGoroutine()

			scenario(t, newConfigFile(t, baseConfig))

			// Goroutines stop cooperatively, so the assertion is that
			// the count comes back rather than that it is back at the
			// instant the scenario returns.
			waitFor(t, func() bool {
				return runtime.NumGoroutine() <= before+2
			}, fmt.Sprintf("goroutines to return to %d (now %d)", before, runtime.NumGoroutine()))
		})
	}
}

func mustOpen(t *testing.T, path string) *dynamicconfig.Config[appConfig] {
	t.Helper()

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(validPort),
		dynamicconfig.WithDebounce[appConfig](10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return cfg
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}

		return false
	})()
}
