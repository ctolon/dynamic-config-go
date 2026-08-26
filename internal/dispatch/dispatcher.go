// Package dispatch delivers callbacks to subscribers on a goroutine of its
// own, in order, with a bounded queue.
//
// Two properties matter more than throughput here.
//
// Ordering: configuration changes describe a sequence, and a subscriber that
// sees generation 12 before generation 11 has been actively misled. One
// worker, one queue, first in first out.
//
// Boundedness: publication must never wait for a subscriber. A subscriber
// that sleeps for a minute is allowed to fall behind; it is not allowed to
// stall a reload or to grow the queue without limit. When the queue is full
// the oldest entry is dropped, because for configuration the newest state is
// the one worth keeping — and the drop is counted, so that falling behind is
// visible rather than silent.
package dispatch

import (
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// PanicHandler is called when a task panics. It runs on the dispatcher
// goroutine and must not panic itself.
type PanicHandler func(recovered any, stack []byte)

// Dispatcher runs submitted tasks one at a time, in submission order.
type Dispatcher struct {
	queue chan func()

	onPanic PanicHandler

	dropped atomic.Uint64

	quit     chan struct{}
	finished chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
}

// New returns a dispatcher with a queue of the given size. A size below one
// is raised to one: a zero-length queue would make every submission a drop.
func New(buffer int, onPanic PanicHandler) *Dispatcher {
	if buffer < 1 {
		buffer = 1
	}

	return &Dispatcher{
		queue:    make(chan func(), buffer),
		onPanic:  onPanic,
		quit:     make(chan struct{}),
		finished: make(chan struct{}),
	}
}

// Start launches the worker. It is safe to call more than once; only the
// first call starts a goroutine.
func (d *Dispatcher) Start() {
	d.startOnce.Do(func() {
		go d.run()
	})
}

// Submit queues a task. It never blocks. If the queue is full the oldest
// queued task is dropped to make room, and the drop counter advances.
func (d *Dispatcher) Submit(task func()) {
	if task == nil {
		return
	}

	select {
	case d.queue <- task:
		return
	default:
	}

	// Full. Discard the oldest entry and take its place. A concurrent
	// worker may have taken that entry already, in which case the receive
	// fails and the send below simply succeeds; the accounting is
	// approximate by at most one under that race, which is the right price
	// for never blocking the reload path.
	select {
	case <-d.queue:
		d.dropped.Add(1)
	default:
	}

	select {
	case d.queue <- task:
	default:
		d.dropped.Add(1)
	}
}

// Dropped reports how many tasks have been discarded because subscribers
// could not keep up.
func (d *Dispatcher) Dropped() uint64 {
	return d.dropped.Load()
}

// Stop shuts the worker down, waiting up to timeout for a task that is
// already running to return. Queued tasks are abandoned: shutdown must be
// bounded even when a subscriber is not.
//
// The contract is deliberately the honest one rather than the tidy one: no
// task starts after the worker has observed the stop, but a task that was
// already dequeued, or one that reaches the worker in the same instant the
// stop does, may still run. Promising instantaneous cancellation would
// require a gate on every dequeue to buy a guarantee nobody can observe.
//
// It reports whether the worker finished within the timeout. Stop is
// idempotent; later calls report the same answer as the first once the
// worker has finished.
func (d *Dispatcher) Stop(timeout time.Duration) bool {
	d.stopOnce.Do(func() {
		close(d.quit)
	})

	// A dispatcher that was never started has no worker to wait for.
	// Claiming the start-once here also means Start can no longer launch
	// one after Stop, which closes the start/stop race.
	started := true

	d.startOnce.Do(func() {
		started = false
	})

	if !started {
		return true
	}

	select {
	case <-d.finished:
		return true

	case <-time.After(timeout):
		return false
	}
}

func (d *Dispatcher) run() {
	defer close(d.finished)

	for {
		// Check for shutdown before looking at the queue at all. A
		// single select over both would let Go pick either when both
		// are ready, so a full queue could keep starting callbacks for
		// an unbounded time after Stop. This makes shutdown win
		// whenever it is already visible, which is what "queued work is
		// abandoned" has to mean to be worth documenting.
		select {
		case <-d.quit:
			return
		default:
		}

		select {
		case <-d.quit:
			return

		case task := <-d.queue:
			d.invoke(task)
		}
	}
}

// invoke runs one task with panic isolation. A subscriber that panics costs
// its own callback and nothing else: the worker survives, the queue
// survives, and the reload machinery upstream never learns of it except
// through the panic handler.
func (d *Dispatcher) invoke(task func()) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		if d.onPanic == nil {
			return
		}

		stack := debug.Stack()

		// The handler is library code, not user code, but a panic here
		// would take down the worker that exists to prevent exactly that.
		defer func() { _ = recover() }()

		d.onPanic(recovered, stack)
	}()

	task()
}
