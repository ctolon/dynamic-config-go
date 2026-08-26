package dispatch

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestNoTaskStartsAfterStopIsObserved pins the documented contract: once
// the worker has seen the stop, it starts nothing further, even with a
// queue full of ready work.
//
// A single select over quit and queue would let Go pick either when both
// are ready, which would make "queued work is abandoned" a statement about
// luck rather than about the implementation. The test is in-package
// because the moment that matters — the stop becoming visible — is not
// observable from outside, and asserting it any other way would be
// asserting a sleep.
func TestNoTaskStartsAfterStopIsObserved(t *testing.T) {
	t.Parallel()

	var started atomic.Int64

	release := make(chan struct{})
	running := make(chan struct{})

	d := New(64, nil)
	d.Start()

	// Wedge the worker, then fill the queue behind it.
	d.Submit(func() {
		close(running)

		<-release
	})

	<-running

	for range 32 {
		d.Submit(func() { started.Add(1) })
	}

	stopped := make(chan bool, 1)

	go func() { stopped <- d.Stop(5 * time.Second) }()

	// Release the running task only once the stop is genuinely visible to
	// the worker, so that what is being tested is the worker's choice
	// rather than the scheduler's.
	waitUntilStopping(t, d)

	close(release)

	if !<-stopped {
		t.Fatal("Stop timed out waiting for the running task")
	}

	if got := started.Load(); got != 0 {
		t.Fatalf("%d queued tasks started after the stop was visible", got)
	}
}

func waitUntilStopping(t *testing.T, d *Dispatcher) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-d.quit:
			return
		default:
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("Stop never signalled the worker")
}
