package dynamicconfig_test

import (
	"context"
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

// FuzzSubscriberOperations drives random sequences of subscription,
// unsubscription and reload. It is looking for panics and deadlocks in the
// registry and the dispatcher, which are the parts where lifetimes overlap
// most.
func FuzzSubscriberOperations(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{1, 1, 1, 1})
	f.Add([]byte{4, 0, 4, 2, 3})
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

		var subs []dynamicconfig.Subscription

		for _, op := range ops {
			switch op % 6 {
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
					// Unsubscribing twice is documented as safe.
					sub.Unsubscribe()
					sub.Unsubscribe()
				}

			case 4:
				_ = cfg.Reload(context.Background())

			case 5:
				_ = cfg.Close()
			}

			if cfg.Current() == nil {
				t.Fatal("the published snapshot was cleared")
			}

			_ = cfg.Status()
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
