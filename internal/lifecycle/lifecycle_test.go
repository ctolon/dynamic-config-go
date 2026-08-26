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

// TestTransitionTable is the state machine written down twice — once as
// code and once as a table — so that a change to one has to be a deliberate
// change to the other.
func TestTransitionTable(t *testing.T) {
	t.Parallel()

	type step struct {
		do   string
		want lifecycle.State
	}

	cases := map[string][]step{
		"ready then watch then back": {
			{"ready", lifecycle.StateReady},
			{"beginWatch", lifecycle.StateWatching},
			{"endWatch", lifecycle.StateReady},
		},

		"close from ready": {
			{"ready", lifecycle.StateReady},
			{"beginClose", lifecycle.StateClosing},
			{"finishClose", lifecycle.StateClosed},
		},

		"close from watching": {
			{"ready", lifecycle.StateReady},
			{"beginWatch", lifecycle.StateWatching},
			{"beginClose", lifecycle.StateClosing},
			{"finishClose", lifecycle.StateClosed},
		},

		"a watcher ending after close cannot revive ready": {
			{"ready", lifecycle.StateReady},
			{"beginWatch", lifecycle.StateWatching},
			{"beginClose", lifecycle.StateClosing},
			{"finishClose", lifecycle.StateClosed},
			{"endWatch", lifecycle.StateClosed},
		},

		"ready is not reachable from closed": {
			{"ready", lifecycle.StateReady},
			{"beginClose", lifecycle.StateClosing},
			{"finishClose", lifecycle.StateClosed},
			{"ready", lifecycle.StateClosed},
		},

		"a second watch does not change the state": {
			{"ready", lifecycle.StateReady},
			{"beginWatch", lifecycle.StateWatching},
			{"beginWatch", lifecycle.StateWatching},
		},

		"watching is not reachable from closed": {
			{"ready", lifecycle.StateReady},
			{"beginClose", lifecycle.StateClosing},
			{"beginWatch", lifecycle.StateClosing},
		},

		"a watcher cannot start before construction finishes": {
			{"beginWatch", lifecycle.StateInitializing},
		},
	}

	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := lifecycle.New()

			for i, s := range steps {
				switch s.do {
				case "ready":
					l.Ready()
				case "beginWatch":
					l.BeginWatch()
				case "endWatch":
					l.EndWatch()
				case "beginClose":
					l.BeginClose()
				case "finishClose":
					l.FinishClose()
				default:
					t.Fatalf("unknown step %q", s.do)
				}

				if got := l.State(); got != s.want {
					t.Fatalf("step %d (%s): state = %s, want %s", i, s.do, got, s.want)
				}
			}
		})
	}
}

func TestRepeatedCallsAreIdempotent(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Ready()

	firsts := 0

	for range 100 {
		if l.BeginClose() {
			firsts++
		}

		l.FinishClose()
	}

	if firsts != 1 {
		t.Fatalf("BeginClose reported itself first %d times, want 1", firsts)
	}

	if l.State() != lifecycle.StateClosed {
		t.Fatalf("state = %s, want closed", l.State())
	}
}

func TestConcurrentWatchAndClose(t *testing.T) {
	t.Parallel()

	for range 100 {
		l := lifecycle.New()
		l.Ready()

		var wg sync.WaitGroup

		wg.Add(3)

		go func() {
			defer wg.Done()

			if l.BeginWatch() == lifecycle.OutcomeOK {
				l.EndWatch()
			}
		}()

		go func() {
			defer wg.Done()

			l.BeginWatch()
		}()

		go func() {
			defer wg.Done()

			if l.BeginClose() {
				l.FinishClose()
			}
		}()

		wg.Wait()

		// Closing is terminal however the race resolved: a watcher that
		// finished afterwards must not have moved the state back.
		if !l.Closed() {
			t.Fatal("the lifecycle did not end closed")
		}
	}
}
