package dynamicconfig

import "sync"

// Subscription is the handle returned by Subscribe and SubscribeErrors.
//
// Unsubscribe is idempotent, safe from any goroutine, and safe after Close.
// Calling it twice is not an error, and neither is calling it from inside
// the handler it cancels.
type Subscription interface {
	// Unsubscribe stops delivery to the handler. A notification already
	// queued or already running may still reach the handler; unsubscribe
	// is a promise about future deliveries, not a barrier.
	Unsubscribe()
}

// registry holds the handlers of one kind, in registration order.
//
// Order matters: dispatch walks the snapshot, so subscribers see changes in
// a stable sequence rather than in Go's randomised map order. Removal is
// rare and delivery is frequent, so the slice is copied for dispatch and
// mutated under the lock.
type registry[H any] struct {
	mu sync.Mutex

	nextID uint64

	entries []entry[H]
}

type entry[H any] struct {
	id      uint64
	handler H
}

func (r *registry[H]) add(handler H) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++

	id := r.nextID

	r.entries = append(r.entries, entry[H]{id: id, handler: handler})

	return id
}

func (r *registry[H]) remove(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.entries {
		if r.entries[i].id != id {
			continue
		}

		// Copy rather than slice in place: the previous backing array may
		// still be walked by a dispatch that took a snapshot, and
		// overwriting it under that reader would be a data race.
		next := make([]entry[H], 0, len(r.entries)-1)
		next = append(next, r.entries[:i]...)
		next = append(next, r.entries[i+1:]...)

		r.entries = next

		return
	}
}

// snapshot returns the handlers to deliver to. The returned slice is not
// shared with the registry, so dispatch can walk it without a lock while
// subscriptions come and go.
func (r *registry[H]) snapshot() []H {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.entries) == 0 {
		return nil
	}

	out := make([]H, len(r.entries))

	for i := range r.entries {
		out[i] = r.entries[i].handler
	}

	return out
}

// clear drops every handler. Close uses it so that a Config held alive by
// an application reference does not keep its subscribers' closures — and
// whatever they capture — reachable.
func (r *registry[H]) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = nil
}

// subscription is the concrete handle. It holds the registry and the id
// rather than a closure so that repeated Unsubscribe calls are cheap and
// obviously idempotent.
type subscription struct {
	once sync.Once

	cancel func()
}

func newSubscription(cancel func()) *subscription {
	return &subscription{cancel: cancel}
}

func (s *subscription) Unsubscribe() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}
