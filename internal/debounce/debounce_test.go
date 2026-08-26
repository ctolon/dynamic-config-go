package debounce_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctolon/dynamic-config-go/internal/debounce"
)

func TestBurstCollapsesToOneRun(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	d := debounce.New(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		d.Run(ctx, func() { runs.Add(1) })
	}()

	// The shape of an editor saving a file: several events, no gaps.
	for range 20 {
		d.Trigger()

		time.Sleep(time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)

	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d, want 1: a burst must collapse", got)
	}

	cancel()
	wg.Wait()
}

func TestSeparatedTriggersRunSeparately(t *testing.T) {
	t.Parallel()

	runs := make(chan struct{}, 8)

	d := debounce.New(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx, func() { runs <- struct{}{} })

	for i := range 3 {
		d.Trigger()

		select {
		case <-runs:
		case <-time.After(2 * time.Second):
			t.Fatalf("trigger %d never ran", i)
		}
	}
}

func TestTriggerDuringActionSchedulesExactlyOneMore(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	started := make(chan struct{}, 1)
	release := make(chan struct{})

	d := debounce.New(0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx, func() {
		if runs.Add(1) == 1 {
			started <- struct{}{}

			<-release
		}
	})

	d.Trigger()

	<-started

	// A storm arrives while the action is busy. At most one further run
	// may follow, however many events there were.
	for range 100 {
		d.Trigger()
	}

	close(release)

	time.Sleep(200 * time.Millisecond)

	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2: one in flight plus one coalesced follow-up", got)
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	t.Parallel()

	d := debounce.New(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())

	returned := make(chan struct{})

	go func() {
		d.Run(ctx, func() {})

		close(returned)
	}()

	d.Trigger()

	cancel()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestTriggerNeverBlocks(t *testing.T) {
	t.Parallel()

	d := debounce.New(time.Hour)

	done := make(chan struct{})

	go func() {
		// Nothing is running the loop, so every trigger after the first
		// has nowhere to go. None of them may block.
		for range 1000 {
			d.Trigger()
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Trigger blocked with no loop draining it")
	}

	if !d.Pending() {
		t.Fatal("no trigger was retained")
	}
}
