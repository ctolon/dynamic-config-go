// Package debounce collapses a burst of triggers into a single action.
//
// A configuration file rewritten by an editor, a deployment tool or a
// kubelet rarely produces one filesystem event. It produces several — write,
// write, chmod, rename, create — describing one logical change. Reloading
// once per event is wasted work, and it publishes generations nobody asked
// for.
//
// The loop here implements the state machine from the proposal:
//
//	IDLE ──trigger──► PENDING ──delay elapsed──► RUNNING ──► IDLE
//	                     ▲                          │
//	                     └────── trigger ───────────┘
//
// The important property is the last edge. A trigger that arrives while the
// action is running is remembered — exactly once, in a one-slot channel — so
// an event storm can never grow a queue. At most one further run is ever
// pending, whatever the event rate.
package debounce

import (
	"context"
	"time"
)

// Debouncer coalesces triggers for one action. The zero value is not
// usable; call New.
type Debouncer struct {
	delay time.Duration

	// trigger holds at most one pending trigger. Sends are non-blocking,
	// so a storm of events costs nothing beyond the first.
	trigger chan struct{}
}

// New returns a debouncer with the given delay. A delay of zero or less
// runs the action as soon as the loop sees the trigger, still coalescing
// whatever arrived while it was busy.
func New(delay time.Duration) *Debouncer {
	return &Debouncer{
		delay:   delay,
		trigger: make(chan struct{}, 1),
	}
}

// Trigger records that the action should run. It never blocks and never
// panics, so it is safe to call from a filesystem event loop.
func (d *Debouncer) Trigger() {
	select {
	case d.trigger <- struct{}{}:
	default:
		// A trigger is already pending; one run will cover both.
	}
}

// Pending reports whether a trigger is waiting to be observed. It exists
// for tests.
func (d *Debouncer) Pending() bool {
	return len(d.trigger) > 0
}

// Run drives the loop until ctx is cancelled, calling action once per
// coalesced burst.
//
// The action runs on this goroutine, which is what bounds the work: a
// second run cannot start until the first returns, and triggers that arrive
// meanwhile fold into a single follow-up.
func (d *Debouncer) Run(ctx context.Context, action func()) {
	var (
		timer  *time.Timer
		expiry <-chan time.Time
	)

	stopTimer := func() {
		if timer == nil {
			return
		}

		if !timer.Stop() {
			// The timer had already fired. Drain it so that a later
			// reset cannot see a stale value.
			select {
			case <-timer.C:
			default:
			}
		}

		expiry = nil
	}

	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return

		case <-d.trigger:
			if d.delay <= 0 {
				action()

				continue
			}

			stopTimer()

			if timer == nil {
				timer = time.NewTimer(d.delay)
			} else {
				timer.Reset(d.delay)
			}

			expiry = timer.C

		case <-expiry:
			expiry = nil

			action()
		}
	}
}
