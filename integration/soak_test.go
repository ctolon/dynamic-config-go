package integration

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

// soakDuration is how long TestSoak runs. Short by default so that
// `go test ./...` stays fast; long when a scheduled run asks for it:
//
//	go test -run TestSoak -soak 30m ./integration/
var soakDuration = flag.Duration("soak", 3*time.Second, "how long TestSoak should run")

// TestSoak runs the whole machine continuously: readers reading, writers
// writing, reloads succeeding and failing, subscribers subscribing and
// going away, all at once.
//
// The short suite proves individual properties in isolation. This is here
// to catch what only appears after a while — a counter that drifts, memory
// that grows with time rather than with configuration size, a watcher that
// stops noticing after enough events, a goroutine that accumulates one per
// hour.
func TestSoak(t *testing.T) {
	// Not parallel: it measures process-wide memory and goroutines.

	if testing.Short() {
		t.Skip("soak test skipped in short mode")
	}

	duration := *soakDuration

	path := filepath.Join(t.TempDir(), "config.yaml")

	// Every document this test writes keeps port == 8000 + len(value), so
	// that a reader can tell a whole snapshot from a torn one.
	write(t, path, document("start", 8000+len("start")))

	cfg, failures := start(t, path, dynamicconfig.WithDebounce[config](20*time.Millisecond))

	// Drain the failure channel: the writer deliberately produces invalid
	// documents, and a full channel would stop being informative.
	var rejected atomic.Int64

	go func() {
		for range failures {
			rejected.Add(1)
		}
	}()

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		reads    atomic.Int64
		writes   atomic.Int64
		torn     atomic.Int64
		previous atomic.Uint64
	)

	baselineGoroutines := runtime.NumGoroutine()

	var baseline runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&baseline)

	// Readers.
	for range 16 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for !stop.Load() {
				current := cfg.Current()
				if current == nil {
					torn.Add(1)

					return
				}

				if current.Port != 8000+len(current.Value) {
					torn.Add(1)

					return
				}

				reads.Add(1)
			}
		}()
	}

	// A generation watcher: monotonicity has to hold for the whole run,
	// not just at the end.
	wg.Add(1)

	go func() {
		defer wg.Done()

		for !stop.Load() {
			generation := cfg.Generation()

			for {
				seen := previous.Load()

				if generation < seen {
					torn.Add(1)

					return
				}

				if generation == seen || previous.CompareAndSwap(seen, generation) {
					break
				}
			}

			time.Sleep(time.Millisecond)
		}
	}()

	// Subscribers coming and going, so the registry never settles.
	wg.Add(1)

	go func() {
		defer wg.Done()

		for !stop.Load() {
			sub := cfg.Subscribe(func(dynamicconfig.Change[config]) {})
			errSub := cfg.SubscribeErrors(func(dynamicconfig.ReloadError) {})

			time.Sleep(5 * time.Millisecond)

			sub.Unsubscribe()
			errSub.Unsubscribe()
		}
	}()

	// The writer: mostly valid, deliberately invalid every third change,
	// so both publication and rejection stay exercised.
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Bursts separated by quiet, which is what a configuration file
		// actually looks like in service: a deployment or an edit
		// rewrites it a few times in quick succession, then nothing
		// happens for a while. A file rewritten continuously faster
		// than the debounce window would simply never reload — the
		// window never expires — which is correct behaviour and a
		// useless soak.
		//
		// Debouncing means only the last write of a burst is ever read,
		// so that is the one that decides whether the burst is
		// accepted. Every other burst ends invalid, which keeps both
		// publication and rejection exercised for the whole run.
		for burst := 0; !stop.Load(); burst++ {
			for i := range 3 {
				value := fmt.Sprintf("soak-%d-%d", burst%1000, i)

				port := 8000 + len(value)

				if i == 2 && burst%2 == 1 {
					port = 70000
				}

				_ = os.WriteFile(path, []byte(document(value, port)), 0o600)

				writes.Add(1)

				time.Sleep(2 * time.Millisecond)
			}

			// Longer than the debounce window, so the burst retires and
			// the next one starts clean.
			time.Sleep(60 * time.Millisecond)
		}
	}()

	time.Sleep(duration)

	stop.Store(true)

	wg.Wait()

	if got := torn.Load(); got != 0 {
		t.Fatalf("%d readers saw a torn, nil or regressed snapshot", got)
	}

	status := cfg.Status()

	// The arithmetic has to survive the whole run, not just the first
	// minute of it.
	if status.Generation != status.SuccessfulReloads+1 {
		t.Fatalf("generation %d does not match %d successful reloads plus the initial load",
			status.Generation, status.SuccessfulReloads)
	}

	if status.SuccessfulReloads == 0 || status.FailedReloads == 0 {
		t.Fatalf("the run exercised too little: %d published, %d rejected", status.SuccessfulReloads, status.FailedReloads)
	}

	if !status.Watching {
		t.Fatal("the watcher stopped at some point during the run")
	}

	// Nothing should accumulate with time. Both bounds are generous on
	// purpose: this is looking for growth, not for a number.
	if grown := runtime.NumGoroutine() - baselineGoroutines; grown > 10 {
		t.Fatalf("goroutines grew by %d over %s", grown, duration)
	}

	runtime.GC()

	var after runtime.MemStats

	runtime.ReadMemStats(&after)

	if after.HeapAlloc > baseline.HeapAlloc+32<<20 {
		t.Fatalf("heap grew from %d to %d bytes over %s", baseline.HeapAlloc, after.HeapAlloc, duration)
	}

	t.Logf("soaked %s: %d reads, %d writes, %d published, %d rejected, %d dropped events",
		duration, reads.Load(), writes.Load(),
		status.SuccessfulReloads, status.FailedReloads, status.DroppedEvents)

	// The configuration still works at the end of the run.
	write(t, path, document("final", 8000+len("final")))

	waitForValue(t, cfg, "final")

	if err := cfg.Reload(context.Background()); err != nil && !errors.Is(err, dynamicconfig.ErrClosed) {
		t.Fatalf("Reload after the run: %v", err)
	}
}
