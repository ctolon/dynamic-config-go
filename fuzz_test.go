package dynamicconfig_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

// FuzzReloadDocument feeds arbitrary bytes to the read/decode/validate
// boundary.
//
// The properties under test are the two that make the last-known-good
// guarantee meaningful: no input may panic, and no input that fails to
// become a configuration may disturb the one already published.
func FuzzReloadDocument(f *testing.F) {
	seeds := []string{
		baseConfig,
		"",
		"\n",
		"server:\n  port: 1\n",
		"server:\n  port: not-a-number\n",
		"server: [1, 2, 3]\n",
		"server:\n  port: 99999999999999999999\n",
		"server: {port: [unterminated\n",
		"features:\n  beta: true\n  alpha: maybe\n",
		"\x00\x01\x02",
		"a: *anchor\n",
		"--- \n--- \n",
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	path := filepath.Join(f.TempDir(), "config.yaml")

	writeConfigF(f, path, baseConfig)

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		f.Fatalf("New: %v", err)
	}

	f.Cleanup(func() { _ = cfg.Close() })

	good := cfg.Current()

	f.Fuzz(func(t *testing.T, document string) {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Skipf("write: %v", err)
		}

		err := cfg.Reload(context.Background())

		current := cfg.Current()

		if current == nil {
			t.Fatal("the published snapshot was cleared")
		}

		if err != nil && current != good {
			t.Fatal("a rejected document replaced the published snapshot")
		}

		if err == nil {
			// The document was accepted, so it becomes the new
			// last-known-good — and it must have satisfied the
			// validator to get here.
			if verr := validPort(current); verr != nil {
				t.Fatalf("a published snapshot does not validate: %v", verr)
			}

			good = current
		}
	})
}

// FuzzLifecycleModel drives random sequences of the operations an
// application can perform and checks the package's invariants after every
// one of them.
//
// Fuzzing a parser looks for inputs that crash it. This looks for
// *orderings* that break a promise — a snapshot that vanishes, a generation
// that moves after close, a Reload that succeeds on a closed
// configuration — which is where a concurrent lifecycle actually goes
// wrong.
func FuzzLifecycleModel(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{1, 1, 1, 1})
	f.Add([]byte{4, 0, 4, 2, 3})
	f.Add([]byte{5, 4, 0, 6, 7})
	f.Add([]byte{2, 2, 2, 2, 2, 2, 2, 2})

	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 64 {
			ops = ops[:64]
		}

		path := filepath.Join(t.TempDir(), "config.yaml")

		writeConfigT(t, path, baseConfig)

		cfg, err := dynamicconfig.New[appConfig](
			dynamicconfig.WithConfigFile[appConfig](path),
			dynamicconfig.WithValidator(validPort),
			dynamicconfig.WithEventBuffer[appConfig](2),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		defer func() { _ = cfg.Close() }()

		// The model: what the invariants are checked against.
		var (
			subs            []dynamicconfig.Subscription
			highest         = cfg.Generation()
			closed          bool
			frozenSnapshot  *appConfig
			frozenGeneraton uint64
		)

		if highest != 1 {
			t.Fatalf("construction published generation %d, want 1", highest)
		}

		for _, op := range ops {
			switch op % 8 {
			case 0:
				subs = append(subs, cfg.Subscribe(func(dynamicconfig.Change[appConfig]) {}))

			case 1:
				subs = append(subs, cfg.SubscribeErrors(func(dynamicconfig.ReloadError) {}))

			case 2:
				if len(subs) > 0 {
					subs[len(subs)-1].Unsubscribe()
					subs = subs[:len(subs)-1]
				}

			case 3:
				for _, sub := range subs {
					// Documented as idempotent, so twice must be as
					// safe as once.
					sub.Unsubscribe()
					sub.Unsubscribe()
				}

			case 4:
				err := cfg.Reload(context.Background())

				if closed && !errors.Is(err, dynamicconfig.ErrClosed) {
					t.Fatalf("Reload on a closed config = %v, want ErrClosed", err)
				}

			case 5:
				// A write that may or may not be valid, so that reloads
				// have something to accept and something to reject.
				writeConfigT(t, path, "server:\n  host: fuzzed\n  port: 9000\n")

			case 6:
				writeConfigT(t, path, "server:\n  port: 70000\n")

			case 7:
				if err := cfg.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}

				if !closed {
					closed = true
					frozenSnapshot = cfg.Current()
					frozenGeneraton = cfg.Generation()
				}
			}

			// Invariants, after every operation.
			current := cfg.Current()

			if current == nil {
				t.Fatal("the published snapshot was cleared")
			}

			generation := cfg.Generation()

			if generation < highest {
				t.Fatalf("generation went backwards: %d after %d", generation, highest)
			}

			highest = generation

			status := cfg.Status()

			if status.Generation != status.SuccessfulReloads+1 {
				t.Fatalf("generation %d does not match %d successful reloads plus the initial load",
					status.Generation, status.SuccessfulReloads)
			}

			if !closed {
				continue
			}

			// Close is terminal: nothing published, nothing moved.
			if current != frozenSnapshot {
				t.Fatal("the snapshot changed after close")
			}

			if generation != frozenGeneraton {
				t.Fatalf("generation moved from %d to %d after close", frozenGeneraton, generation)
			}

			if !status.Closed {
				t.Fatal("status does not report a closed config as closed")
			}

			if err := cfg.Watch(context.Background()); !errors.Is(err, dynamicconfig.ErrClosed) {
				t.Fatalf("Watch on a closed config = %v, want ErrClosed", err)
			}
		}
	})
}

func writeConfigF(f *testing.F, path, contents string) {
	f.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		f.Fatalf("write %s: %v", path, err)
	}
}

func writeConfigT(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
