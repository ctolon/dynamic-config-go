package dynamicconfig

import (
	"sync"
	"testing"

	"github.com/spf13/viper"
)

// viperInstance keeps the setup helper below readable.
type viperInstance = viper.Viper

// These tests reach inside the package on purpose. Handler retention is not
// observable through the public API — a retained handler that never runs
// looks exactly like no handler at all — and "a closed configuration
// retains nothing" is a memory-lifetime promise worth checking rather than
// assuming.

func TestRegistryAddAndRemove(t *testing.T) {
	t.Parallel()

	var reg registry[func()]

	first, ok := reg.add(func() {})
	if !ok {
		t.Fatal("add refused a handler on an open registry")
	}

	second, ok := reg.add(func() {})
	if !ok {
		t.Fatal("add refused the second handler")
	}

	if len(reg.snapshot()) != 2 {
		t.Fatalf("registry holds %d handlers, want 2", len(reg.snapshot()))
	}

	reg.remove(first)

	if len(reg.snapshot()) != 1 {
		t.Fatalf("registry holds %d handlers after one removal, want 1", len(reg.snapshot()))
	}

	// Removing twice, and removing an id that was never there, are both
	// no-ops rather than errors.
	reg.remove(first)
	reg.remove(second + 100)

	if len(reg.snapshot()) != 1 {
		t.Fatalf("a repeated removal changed the registry")
	}
}

func TestRegistryCloseRetainsNothing(t *testing.T) {
	t.Parallel()

	var reg registry[func()]

	reg.add(func() {})
	reg.add(func() {})

	reg.close()

	if got := len(reg.entries); got != 0 {
		t.Fatalf("registry retained %d handlers through close", got)
	}

	if _, ok := reg.add(func() {}); ok {
		t.Fatal("a closed registry accepted a handler")
	}

	if got := len(reg.entries); got != 0 {
		t.Fatalf("a closed registry retained %d handlers", got)
	}
}

func TestSubscribeAfterCloseDoesNotRetainHandler(t *testing.T) {
	t.Parallel()

	cfg := newSealedTestConfig(t)

	if err := cfg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A handler registered after close must not be kept alive by the
	// Config: whatever it captured — a pool, a client, a cache — would
	// stay reachable for as long as anything held the configuration.
	sub := cfg.Subscribe(func(Change[testConfig]) {})
	errSub := cfg.SubscribeErrors(func(ReloadError) {})

	if got := len(cfg.changeSubs.entries); got != 0 {
		t.Fatalf("closed config retained %d change handlers", got)
	}

	if got := len(cfg.errorSubs.entries); got != 0 {
		t.Fatalf("closed config retained %d error handlers", got)
	}

	// The returned handles are still safe to use.
	sub.Unsubscribe()
	sub.Unsubscribe()
	errSub.Unsubscribe()
}

func TestConcurrentSubscribeAndCloseDoesNotRetainHandlers(t *testing.T) {
	t.Parallel()

	for range 50 {
		cfg := newSealedTestConfig(t)

		var wg sync.WaitGroup

		for range 8 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				cfg.Subscribe(func(Change[testConfig]) {})
				cfg.SubscribeErrors(func(ReloadError) {})
			}()
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = cfg.Close()
		}()

		wg.Wait()

		// Whichever way the race went, the registries are terminal
		// afterwards: the decision to retain is made inside the
		// registry's own lock, so there is no window between "closed"
		// and "still accepting".
		if got := len(cfg.changeSubs.entries); got != 0 {
			t.Fatalf("closed config retained %d change handlers", got)
		}

		if got := len(cfg.errorSubs.entries); got != 0 {
			t.Fatalf("closed config retained %d error handlers", got)
		}
	}
}

type testConfig struct {
	Server struct {
		Port int `mapstructure:"port"`
	} `mapstructure:"server"`
}

func newSealedTestConfig(t *testing.T) *Config[testConfig] {
	t.Helper()

	cfg, err := NewSealed[testConfig](
		WithAllowMissingFile[testConfig](true),
		WithViperSetup[testConfig](func(v *viperInstance) error {
			v.SetDefault("server.port", 8080)

			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewSealed: %v", err)
	}

	t.Cleanup(func() { _ = cfg.Close() })

	return cfg
}
