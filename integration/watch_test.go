// Package integration exercises the package against a real filesystem.
//
// The unit tests can prove that a rejected candidate leaves the snapshot
// alone; only these can prove that a rename-into-place, a projected-volume
// swap or a deleted file behave the way the documentation claims. They use
// real directories, real writes and the real watcher.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

type config struct {
	Value string `mapstructure:"value"`
	Port  int    `mapstructure:"port"`
}

func validate(c *config) error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}

	return nil
}

func document(value string, port int) string {
	return fmt.Sprintf("value: %s\nport: %d\n", value, port)
}

func write(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// replace writes contents to a temporary file in the same directory and
// renames it over path — the way editors and deployment tools update files,
// and the reason the watcher watches a directory rather than an inode.
func replace(t *testing.T, path, contents string) {
	t.Helper()

	tmp := path + ".tmp"

	write(t, tmp, contents)

	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename %s: %v", tmp, err)
	}
}

// start builds a watched configuration over path and returns it with a
// channel of reload failures. The watcher is stopped when the test ends.
func start(t *testing.T, path string, opts ...dynamicconfig.Option[config]) (*dynamicconfig.Config[config], <-chan dynamicconfig.ReloadError) {
	t.Helper()

	all := append([]dynamicconfig.Option[config]{
		dynamicconfig.WithConfigFile[config](path),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithDebounce[config](20 * time.Millisecond),
	}, opts...)

	cfg, err := dynamicconfig.New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	failures := make(chan dynamicconfig.ReloadError, 32)

	cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
		select {
		case failures <- e:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan error, 1)

	go func() { stopped <- cfg.Watch(ctx) }()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-stopped:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, dynamicconfig.ErrClosed) {
				t.Errorf("Watch: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Watch did not return")
		}

		_ = cfg.Close()
	})

	waitFor(t, func() bool { return cfg.Status().Watching }, "the watcher to start")

	return cfg, failures
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func waitForValue(t *testing.T, cfg *dynamicconfig.Config[config], want string) {
	t.Helper()

	waitFor(t, func() bool { return cfg.Current().Value == want }, "the snapshot to become "+want)
}

func waitForFailure(t *testing.T, failures <-chan dynamicconfig.ReloadError, stage dynamicconfig.ReloadStage) dynamicconfig.ReloadError {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case e := <-failures:
			if e.Stage == stage {
				return e
			}

		case <-deadline:
			t.Fatalf("no %s failure was reported", stage)
		}
	}
}

func TestWriteIsPickedUp(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("first", 8080))

	cfg, _ := start(t, path)

	write(t, path, document("second", 8081))

	waitForValue(t, cfg, "second")

	if got := cfg.Current().Port; got != 8081 {
		t.Fatalf("port = %d, want 8081", got)
	}

	if cfg.Status().SuccessfulReloads == 0 {
		t.Fatal("the reload was not counted")
	}
}

func TestAtomicReplaceIsPickedUp(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("first", 8080))

	cfg, _ := start(t, path)

	replace(t, path, document("replaced", 8082))

	waitForValue(t, cfg, "replaced")
}

func TestInvalidUpdateKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("good", 8080))

	cfg, failures := start(t, path)

	good := cfg.Current()

	write(t, path, document("bad", 70000))

	failure := waitForFailure(t, failures, dynamicconfig.StageValidation)

	if failure.Source != dynamicconfig.ReloadSourceFile {
		t.Fatalf("source = %q, want %q", failure.Source, dynamicconfig.ReloadSourceFile)
	}

	if cfg.Current() != good {
		t.Fatal("an invalid file replaced the published snapshot")
	}

	// valid → invalid → valid: the configuration recovers on its own.
	write(t, path, document("recovered", 8083))

	waitForValue(t, cfg, "recovered")
}

func TestUnparseableUpdateKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("good", 8080))

	cfg, failures := start(t, path)

	good := cfg.Current()

	// The shape of a half-written file: a key with no value yet.
	write(t, path, "value: partial\nport:\n  nested: [unterminated\n")

	waitForFailure(t, failures, dynamicconfig.StageRead)

	if cfg.Current() != good {
		t.Fatal("an unparseable file replaced the published snapshot")
	}

	write(t, path, document("whole", 8084))

	waitForValue(t, cfg, "whole")
}

func TestDeletedFileKeepsSnapshotAndRecreationPublishes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("present", 8080))

	cfg, failures := start(t, path)

	present := cfg.Current()

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	waitForFailure(t, failures, dynamicconfig.StageRead)

	if cfg.Current() != present {
		t.Fatal("deleting the file cleared the published snapshot")
	}

	write(t, path, document("restored", 8085))

	waitForValue(t, cfg, "restored")
}

func TestKubernetesProjectedVolumeUpdate(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		// Projected volumes exist on Linux nodes and nowhere else, so
		// this is verified where it runs. It is also not reproducible
		// elsewhere: kqueue follows a watched symlink to its target, so
		// renaming a new link over ..data touches nothing the watcher
		// holds and produces no event at all on macOS.
		t.Skip("Kubernetes projected volumes are a Linux mechanism")
	}

	dir := t.TempDir()

	// The layout a kubelet builds for a ConfigMap mount: a timestamped
	// data directory, a ..data symlink pointing at it, and the
	// configuration file as a symlink through ..data.
	publish := func(stamp, contents string) {
		t.Helper()

		dataDir := filepath.Join(dir, ".."+stamp)

		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dataDir, err)
		}

		write(t, filepath.Join(dataDir, "config.yaml"), contents)

		tmpLink := filepath.Join(dir, "..data_tmp")

		if err := os.Symlink(dataDir, tmpLink); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		// The swap itself: a rename over ..data, which is atomic and is
		// the only event the mount produces.
		if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
			t.Fatalf("rename ..data: %v", err)
		}
	}

	publish("2026_08_26_10_00_00", document("mounted", 8080))

	path := filepath.Join(dir, "config.yaml")

	if err := os.Symlink(filepath.Join(dir, "..data", "config.yaml"), path); err != nil {
		t.Fatalf("symlink config: %v", err)
	}

	cfg, _ := start(t, path)

	if got := cfg.Current().Value; got != "mounted" {
		t.Fatalf("value = %q, want the mounted configuration", got)
	}

	// kubectl edit configmap, some seconds later.
	publish("2026_08_26_10_05_00", document("updated", 8086))

	waitForValue(t, cfg, "updated")

	if got := cfg.Current().Port; got != 8086 {
		t.Fatalf("port = %d, want 8086", got)
	}
}

func TestChangeBetweenLoadAndWatchIsNotMissed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("loaded", 8080))

	cfg, err := dynamicconfig.New[config](
		dynamicconfig.WithConfigFile[config](path),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithDebounce[config](20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	// The gap: the file changes after the initial load and before the
	// watcher exists, so no event will ever describe this write.
	time.Sleep(10 * time.Millisecond)
	write(t, path, document("changed-in-the-gap", 8087))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = cfg.Watch(ctx) }()

	waitForValue(t, cfg, "changed-in-the-gap")
}

func TestPermissionChangeBeforeWatchIsNotMissed(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("loaded", 8080))

	cfg, err := dynamicconfig.New[config](
		dynamicconfig.WithConfigFile[config](path),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithDebounce[config](20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	failures := make(chan dynamicconfig.ReloadError, 8)

	cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
		select {
		case failures <- e:
		default:
		}
	})

	// A change in the gap between loading and watching that leaves the
	// modification time alone. Only the mode moved, so this is what the
	// stamp has to notice for the gap check to be worth having.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = cfg.Watch(ctx) }()

	waitForFailure(t, failures, dynamicconfig.StageRead)

	if cfg.Current().Value != "loaded" {
		t.Fatal("an unreadable file disturbed the published snapshot")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
}

func TestUnchangedFileDoesNotRepublishOnWatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("stable", 8080))

	cfg, _ := start(t, path)

	// Starting a watcher over a file nobody touched must not manufacture
	// a generation.
	time.Sleep(200 * time.Millisecond)

	if got := cfg.Generation(); got != 1 {
		t.Fatalf("generation = %d, want 1: watching an unchanged file republished it", got)
	}
}

func TestRapidWritesCoalesce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("first", 8080))

	const (
		writes   = 25
		debounce = 250 * time.Millisecond
	)

	cfg, _ := start(t, path, dynamicconfig.WithDebounce[config](debounce))

	began := time.Now()

	for i := range writes {
		write(t, path, document("burst", 9000+i))

		time.Sleep(2 * time.Millisecond)
	}

	burst := time.Since(began)

	waitFor(t, func() bool { return cfg.Current().Port == 9024 }, "the last write to be published")

	// A debounce window retires at most one reload, so a burst spanning n
	// windows can produce at most n reloads plus one for the tail. On a
	// machine quick enough to write all of this inside one window — which
	// is the case the debounce exists for — that bound is one reload for
	// twenty-five writes. On a loaded CI runner under -race the burst
	// genuinely spans several windows, and the same arithmetic still
	// holds, which is what makes this an assertion about coalescing
	// rather than about the machine it runs on.
	limit := uint64(burst/debounce) + 2

	if got := cfg.Status().SuccessfulReloads; got > limit {
		t.Fatalf("successful reloads = %d for %d writes spanning %s (limit %d)",
			got, writes, burst.Round(time.Millisecond), limit)
	}
}

func TestEventStormIsBounded(t *testing.T) {
	// Deliberately not parallel. It counts goroutines, and
	// runtime.NumGoroutine is a property of the process rather than of a
	// test, so a sibling running alongside would be measured too.

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("first", 8080))

	cfg, _ := start(t, path, dynamicconfig.WithDebounce[config](0))

	goroutinesBefore := runtime.NumGoroutine()

	for i := range 300 {
		write(t, path, document("storm", 9000+(i%100)))
	}

	waitFor(t, func() bool { return cfg.Current().Value == "storm" }, "the storm to be published")

	// The point of coalescing: an unbounded event rate must not produce
	// an unbounded amount of anything. What is asserted is that the count
	// comes back down, not that it never rose — a reload in flight owns a
	// goroutine or two by design, and a sample taken at an arbitrary
	// instant would be measuring the scheduler.
	waitFor(t, func() bool {
		return runtime.NumGoroutine()-goroutinesBefore <= 10
	}, "goroutines to settle back to their baseline after the storm")

	// How many reloads a storm produces is the operating system's
	// business: some watchers report one event per write and some report
	// several, and coalescing bounds the queue rather than the total.
	// What must be true is that the storm ends — once the writes stop,
	// the reloading stops too, rather than feeding itself.
	var (
		last     uint64
		stable   int
		deadline = time.Now().Add(10 * time.Second)
	)

	for time.Now().Before(deadline) && stable < 3 {
		current := cfg.Status().SuccessfulReloads

		if current == last {
			stable++
		} else {
			stable = 0
			last = current
		}

		time.Sleep(100 * time.Millisecond)
	}

	if stable < 3 {
		t.Fatalf("reloads were still arriving after the writes stopped (at %d)", last)
	}

	status := cfg.Status()

	if status.Generation != status.SuccessfulReloads+1 {
		t.Fatalf("generation %d does not match %d successful reloads plus the initial load",
			status.Generation, status.SuccessfulReloads)
	}
}

func TestUnreadableFileKeepsSnapshot(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("readable", 8080))

	cfg, failures := start(t, path)

	readable := cfg.Current()

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	waitForFailure(t, failures, dynamicconfig.StageRead)

	if cfg.Current() != readable {
		t.Fatal("an unreadable file replaced the published snapshot")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}

	write(t, path, document("readable-again", 8088))

	waitForValue(t, cfg, "readable-again")
}

func TestEmptyFileFollowsTheOrdinaryRules(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("populated", 8080))

	cfg, failures := start(t, path)

	populated := cfg.Current()

	// An empty document parses; it just does not validate, because port
	// becomes zero. No special case, no magic: read, decode, validate.
	write(t, path, "")

	waitForFailure(t, failures, dynamicconfig.StageValidation)

	if cfg.Current() != populated {
		t.Fatal("an empty file replaced the published snapshot")
	}
}

func TestConcurrentReadersDuringReloads(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	// Every document this test writes keeps port == 8000 + len(value), so
	// that a reader can tell a whole snapshot from a torn one by looking
	// at two fields.
	write(t, path, document("start", 8000+len("start")))

	cfg, err := dynamicconfig.New[config](
		dynamicconfig.WithConfigFile[config](path),
		dynamicconfig.WithValidator(validate),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	const readers = 64

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		observed atomic.Int64
	)

	for range readers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			var highest uint64

			for !stop.Load() {
				current := cfg.Current()
				if current == nil {
					t.Error("Current() returned nil while reloads were running")

					return
				}

				// A snapshot is whole or it is not published: a value
				// and a port always come from the same document.
				if want := 8000 + len(current.Value); current.Port != want {
					t.Errorf("torn snapshot: value %q with port %d", current.Value, current.Port)

					return
				}

				generation := cfg.Generation()
				if generation < highest {
					t.Errorf("generation went backwards: %d after %d", generation, highest)

					return
				}

				highest = generation

				observed.Add(1)
			}
		}()
	}

	// Alternate good and bad documents, so that readers see both
	// publications and rejections.
	for i := range 200 {
		value := strings.Repeat("x", 80+(i%20))

		if i%3 == 0 {
			write(t, path, document(value, 70000))
		} else {
			write(t, path, document(value, 8000+len(value)))
		}

		_ = cfg.Reload(context.Background())
	}

	stop.Store(true)

	wg.Wait()

	if observed.Load() == 0 {
		t.Fatal("no reads were observed")
	}

	status := cfg.Status()

	if status.Generation != status.SuccessfulReloads+1 {
		t.Fatalf("generation %d does not match %d successful reloads plus the initial load",
			status.Generation, status.SuccessfulReloads)
	}

	if status.FailedReloads == 0 {
		t.Fatal("no reload was rejected, so the test proved nothing about rejection")
	}
}

// TestWritesOutpacingReloadStayBounded generates changes faster than the
// reload transaction can retire them.
//
// A validator that takes its time is the lever: every reload now costs
// real milliseconds while writes cost microseconds, so the queue between
// them is under genuine pressure. What must hold is that the pressure has
// nowhere to accumulate — one reload runs, at most one waits, and the last
// write still wins in the end.
func TestWritesOutpacingReloadStayBounded(t *testing.T) {
	// Not parallel: it counts goroutines.

	path := filepath.Join(t.TempDir(), "config.yaml")

	write(t, path, document("first", 8080))

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	slowValidator := func(c *config) error {
		running := inFlight.Add(1)

		defer inFlight.Add(-1)

		for {
			highest := peak.Load()
			if running <= highest || peak.CompareAndSwap(highest, running) {
				break
			}
		}

		// Slow enough that writes comfortably outpace reloads.
		time.Sleep(5 * time.Millisecond)

		return validate(c)
	}

	cfg, _ := start(t, path,
		dynamicconfig.WithValidator(slowValidator),
		dynamicconfig.WithDebounce[config](0),
	)

	goroutinesBefore := runtime.NumGoroutine()

	for i := range 200 {
		write(t, path, document("outpaced", 9000+(i%50)))
	}

	write(t, path, document("last", 9100))

	waitFor(t, func() bool { return cfg.Current().Value == "last" }, "the last write to win")

	// Serialisation is the first bound: two reloads never overlap, so a
	// storm cannot multiply the work in flight.
	if got := peak.Load(); got > 1 {
		t.Fatalf("%d reloads ran concurrently; reloads must be serialised", got)
	}

	// Coalescing is the second: the events that arrived while a reload
	// was running collapsed into at most one further reload, rather than
	// into a queue that grows with the event rate.
	waitFor(t, func() bool {
		return runtime.NumGoroutine()-goroutinesBefore <= 10
	}, "goroutines to settle after writes outpaced reloads")

	status := cfg.Status()

	if status.Generation != status.SuccessfulReloads+1 {
		t.Fatalf("generation %d does not match %d successful reloads plus the initial load",
			status.Generation, status.SuccessfulReloads)
	}
}

// TestWatchingEveryLayer proves a layered configuration watches all of its
// files: a change to any one of them reloads the whole set, and the layering
// still applies afterwards.
func TestWatchingEveryLayer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	base := filepath.Join(dir, "config.yaml")
	secret := filepath.Join(dir, "secret.yaml")

	write(t, base, "value: base\nport: 8080\n")
	write(t, secret, "port: 9443\n")

	cfg, err := dynamicconfig.New[config](
		dynamicconfig.WithConfigFile[config](base),
		dynamicconfig.WithConfigFile[config](secret),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithDebounce[config](20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan error, 1)

	go func() { stopped <- cfg.Watch(ctx) }()

	t.Cleanup(func() {
		cancel()
		<-stopped
		_ = cfg.Close()
	})

	waitFor(t, func() bool { return cfg.Status().Watching }, "the watcher to start")

	if got := cfg.Current().Port; got != 9443 {
		t.Fatalf("port = %d, want the upper layer's value", got)
	}

	// A change to the lower layer.
	write(t, base, "value: base-changed\nport: 8080\n")

	waitFor(t, func() bool { return cfg.Current().Value == "base-changed" }, "the base layer change")

	if got := cfg.Current().Port; got != 9443 {
		t.Fatalf("port = %d: the upper layer stopped applying after a base reload", got)
	}

	// A change to the upper layer.
	write(t, secret, "port: 9444\n")

	waitFor(t, func() bool { return cfg.Current().Port == 9444 }, "the upper layer change")

	if got := cfg.Current().Value; got != "base-changed" {
		t.Fatalf("value = %q: the base layer was lost after an upper reload", got)
	}
}

// TestLayersInSeparateDirectories covers the deployment where a secret is
// mounted somewhere else entirely — a Kubernetes Secret volume next to a
// ConfigMap volume, most often — so the watcher has two directories to
// follow rather than one.
func TestLayersInSeparateDirectories(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	secretDir := t.TempDir()

	base := filepath.Join(configDir, "config.yaml")
	secret := filepath.Join(secretDir, "secret.yaml")

	write(t, base, "value: base\nport: 8080\n")
	write(t, secret, "port: 9443\n")

	cfg, err := dynamicconfig.New[config](
		dynamicconfig.WithConfigFile[config](base),
		dynamicconfig.WithConfigFile[config](secret),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithDebounce[config](20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan error, 1)

	go func() { stopped <- cfg.Watch(ctx) }()

	t.Cleanup(func() {
		cancel()
		<-stopped
		_ = cfg.Close()
	})

	waitFor(t, func() bool { return cfg.Status().Watching }, "the watcher to start")

	// Each directory is watched, so a rename-into-place in either one is
	// noticed.
	replace(t, secret, "port: 9445\n")

	waitFor(t, func() bool { return cfg.Current().Port == 9445 }, "the secret directory change")

	replace(t, base, "value: elsewhere\nport: 8080\n")

	waitFor(t, func() bool { return cfg.Current().Value == "elsewhere" }, "the config directory change")

	if got := cfg.Current().Port; got != 9445 {
		t.Fatalf("port = %d: layering broke across directories", got)
	}
}
