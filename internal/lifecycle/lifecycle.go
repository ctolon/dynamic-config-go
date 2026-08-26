// Package lifecycle implements the state machine that governs a
// configuration object's life: initialising, ready, watching, closing,
// closed.
//
// The states, and the transitions between them, are the ones drawn in the
// project proposal:
//
//	New() ──► INITIALIZING ──► READY ──► WATCHING
//	                            │           │
//	                            └───────────┴──► CLOSING ──► CLOSED
//
// Keeping the machine here — rather than as a handful of atomic booleans
// spread across the package — is what makes "Close is idempotent" and "only
// one watcher at a time" properties of a single object instead of properties
// of every call site that has to remember them.
//
// The package returns transition outcomes rather than errors: the exported
// sentinels belong to the public package, and an internal package that
// invented its own would either duplicate them or invert the dependency.
package lifecycle

import (
	"sync"
	"sync/atomic"
)

// State is one node of the lifecycle machine.
type State int32

// The lifecycle states.
const (
	// StateInitializing is the state during construction, before the
	// initial load has published a snapshot. No object in this state is
	// ever visible to a caller: construction either reaches StateReady or
	// returns an error and the object is discarded.
	StateInitializing State = iota

	// StateReady means constructed, with a published snapshot, and no
	// watcher running.
	StateReady

	// StateWatching means a watcher goroutine owns the filesystem watch.
	StateWatching

	// StateClosing means Close has begun but has not finished releasing
	// resources.
	StateClosing

	// StateClosed is terminal.
	StateClosed
)

// String renders the state for diagnostics.
func (s State) String() string {
	switch s {
	case StateInitializing:
		return "initializing"
	case StateReady:
		return "ready"
	case StateWatching:
		return "watching"
	case StateClosing:
		return "closing"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Outcome reports what a requested transition did.
type Outcome int

// Transition outcomes.
const (
	// OutcomeOK means the transition happened.
	OutcomeOK Outcome = iota

	// OutcomeClosed means the object is closing or closed, so the
	// transition will never happen.
	OutcomeClosed

	// OutcomeBusy means the transition is refused because the resource is
	// already taken — today, a second watcher.
	OutcomeBusy
)

// Lifecycle is the state machine. The zero value is not usable; call New.
type Lifecycle struct {
	state atomic.Int32

	mu sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
}

// New returns a lifecycle in StateInitializing.
func New() *Lifecycle {
	return &Lifecycle{done: make(chan struct{})}
}

// State reports the current state. It is safe to call concurrently, and is
// a snapshot: the state may have moved on by the time the caller reads it.
func (l *Lifecycle) State() State {
	return State(l.state.Load())
}

// Closed reports whether Close has been called. It stays true from the
// instant closing begins, so that in-flight work can abandon early rather
// than racing the final transition.
func (l *Lifecycle) Closed() bool {
	s := l.State()

	return s == StateClosing || s == StateClosed
}

// Watching reports whether a watcher currently owns the watch.
func (l *Lifecycle) Watching() bool {
	return l.State() == StateWatching
}

// Done is closed when Close begins. Long-running work selects on it to
// learn that it should stop.
func (l *Lifecycle) Done() <-chan struct{} {
	return l.done
}

// Ready moves INITIALIZING to READY. It is called once, by construction,
// after the initial snapshot has been published.
func (l *Lifecycle) Ready() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if State(l.state.Load()) == StateInitializing {
		l.state.Store(int32(StateReady))
	}
}

// BeginWatch moves READY to WATCHING.
//
// It returns OutcomeBusy if a watcher is already running, which is what
// makes the single-watcher policy a property of the machine, and
// OutcomeClosed once the object is closing.
func (l *Lifecycle) BeginWatch() Outcome {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch State(l.state.Load()) {
	case StateReady:
		l.state.Store(int32(StateWatching))

		return OutcomeOK

	case StateWatching:
		return OutcomeBusy

	case StateInitializing:
		// A watcher cannot start before construction finishes; no caller
		// holds a reference to the object yet, so this is unreachable in
		// practice. Refusing is the safe answer.
		return OutcomeBusy

	default:
		return OutcomeClosed
	}
}

// EndWatch moves WATCHING back to READY when a watcher stops of its own
// accord. If closing has already begun the state is left alone: CLOSING and
// CLOSED are terminal.
func (l *Lifecycle) EndWatch() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if State(l.state.Load()) == StateWatching {
		l.state.Store(int32(StateReady))
	}
}

// BeginClose moves the machine to CLOSING and closes Done. It reports
// whether this call is the one that started closing, so that the caller can
// make Close idempotent without a second flag.
func (l *Lifecycle) BeginClose() bool {
	first := false

	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.state.Store(int32(StateClosing))
		l.mu.Unlock()

		close(l.done)

		first = true
	})

	return first
}

// FinishClose moves CLOSING to CLOSED.
func (l *Lifecycle) FinishClose() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.state.Store(int32(StateClosed))
}
