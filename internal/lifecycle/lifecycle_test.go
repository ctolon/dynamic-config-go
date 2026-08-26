package lifecycle_test

import (
	"sync"
	"testing"

	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
)

func TestHappyPath(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()

	if l.State() != lifecycle.StateInitializing {
		t.Fatalf("state = %s, want initializing", l.State())
	}

	l.Ready()

	if l.State() != lifecycle.StateReady {
		t.Fatalf("state = %s, want ready", l.State())
	}

	if got := l.BeginWatch(); got != lifecycle.OutcomeOK {
		t.Fatalf("BeginWatch = %v, want OK", got)
	}

	if !l.Watching() {
		t.Fatal("Watching() is false while watching")
	}

	l.EndWatch()

	if l.State() != lifecycle.StateReady {
		t.Fatalf("state = %s, want ready after EndWatch", l.State())
	}
}

func TestSecondWatcherIsRefused(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Ready()

	if got := l.BeginWatch(); got != lifecycle.OutcomeOK {
		t.Fatalf("first BeginWatch = %v", got)
	}

	if got := l.BeginWatch(); got != lifecycle.OutcomeBusy {
		t.Fatalf("second BeginWatch = %v, want busy", got)
	}
}

func TestCloseIsIdempotentAndTerminal(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Ready()

	if !l.BeginClose() {
		t.Fatal("the first BeginClose did not report itself as first")
	}

	if l.BeginClose() {
		t.Fatal("a later BeginClose reported itself as first")
	}

	select {
	case <-l.Done():
	default:
		t.Fatal("Done was not closed")
	}

	if !l.Closed() {
		t.Fatal("Closed() is false while closing")
	}

	if got := l.BeginWatch(); got != lifecycle.OutcomeClosed {
		t.Fatalf("BeginWatch after close = %v, want closed", got)
	}

	l.FinishClose()

	if l.State() != lifecycle.StateClosed {
		t.Fatalf("state = %s, want closed", l.State())
	}

	// A watcher that ends after closing must not resurrect the ready
	// state.
	l.EndWatch()

	if l.State() != lifecycle.StateClosed {
		t.Fatalf("state = %s after EndWatch on a closed lifecycle", l.State())
	}
}

func TestConcurrentTransitions(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Ready()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		okRuns int
	)

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if l.BeginWatch() == lifecycle.OutcomeOK {
				mu.Lock()
				okRuns++
				mu.Unlock()

				l.EndWatch()
			}
		}()
	}

	wg.Wait()

	if okRuns == 0 {
		t.Fatal("no goroutine acquired the watch")
	}

	l.BeginClose()
	l.FinishClose()
}
