package dispatch_test

import (
	"sync"
	"testing"
	"time"

	"github.com/ctolon/dynamic-config-go/internal/dispatch"
)

func TestTasksRunInOrder(t *testing.T) {
	t.Parallel()

	const count = 100

	seen := make(chan int, count)

	d := dispatch.New(count, nil)
	d.Start()

	defer d.Stop(time.Second)

	for i := range count {
		d.Submit(func() { seen <- i })
	}

	for want := range count {
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("task %d ran at position %d: order was not preserved", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d tasks ran", want)
		}
	}
}

func TestFullQueueDropsOldestAndCounts(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})

	d := dispatch.New(2, nil)
	d.Start()

	// Wedge the worker so the queue fills.
	d.Submit(func() {
		close(started)

		<-release
	})

	<-started

	var mu sync.Mutex

	var ran []int

	for i := range 10 {
		d.Submit(func() {
			mu.Lock()
			ran = append(ran, i)
			mu.Unlock()
		})
	}

	if got := d.Dropped(); got == 0 {
		t.Fatal("a full queue dropped nothing")
	}

	close(release)

	if !d.Stop(2 * time.Second) {
		t.Fatal("the dispatcher did not stop")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(ran) > 2 {
		t.Fatalf("%d tasks ran from a queue of depth 2", len(ran))
	}

	// Dropping the oldest keeps the newest: whatever ran must be from the
	// tail of the submissions.
	for _, i := range ran {
		if i < 8 {
			t.Fatalf("task %d ran, but the newest tasks should have survived", i)
		}
	}
}

func TestPanicIsIsolated(t *testing.T) {
	t.Parallel()

	panics := make(chan any, 1)
	after := make(chan struct{}, 1)

	d := dispatch.New(4, func(recovered any, stack []byte) {
		if len(stack) == 0 {
			t.Error("no stack was captured")
		}

		panics <- recovered
	})

	d.Start()

	defer d.Stop(time.Second)

	d.Submit(func() { panic("boom") })
	d.Submit(func() { after <- struct{}{} })

	select {
	case got := <-panics:
		if got != "boom" {
			t.Fatalf("panic value = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the panic handler never ran")
	}

	select {
	case <-after:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker died with the panicking task")
	}
}

func TestStopWithoutStart(t *testing.T) {
	t.Parallel()

	d := dispatch.New(1, nil)

	if !d.Stop(time.Second) {
		t.Fatal("stopping a dispatcher that never started should not wait")
	}

	// Start after Stop must not resurrect the worker.
	d.Start()

	d.Submit(func() { t.Error("a task ran after Stop") })

	time.Sleep(100 * time.Millisecond)
}

func TestStopIsBoundedByTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})

	d := dispatch.New(1, nil)
	d.Start()

	d.Submit(func() {
		close(started)

		<-release
	})

	<-started

	if d.Stop(100 * time.Millisecond) {
		t.Fatal("Stop claimed success while a task was still running")
	}

	close(release)
}

func TestSubmitNilIsIgnored(t *testing.T) {
	t.Parallel()

	d := dispatch.New(1, nil)
	d.Start()

	defer d.Stop(time.Second)

	d.Submit(nil)
}
