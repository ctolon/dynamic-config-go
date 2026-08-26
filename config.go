package dynamicconfig

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ctolon/dynamic-config-go/internal/dispatch"
	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
	"github.com/spf13/viper"
)

// Config publishes a validated, typed snapshot of T and replaces it
// atomically when the configuration on disk changes.
//
// A Config is built in one of two shapes. An open one keeps the Viper
// instance reachable through Viper, so that an application can go on using
// everything Viper does well; a sealed one does not, so that the engine
// cannot be reached, mutated or raced from anywhere the Config travels to.
// See New and NewSealed.
//
// In an open Config the two halves answer different questions, and the
// difference is the single most important thing to understand about the
// package:
//
//	cfg.Viper().GetInt("server.port")  // Viper's current, mutable state
//	cfg.Current().Server.Port          // the last snapshot that decoded and validated
//
// They can disagree, and when they do, Current is the one the application
// should believe. Setting a bad value on Viper changes what Viper reports
// immediately; it changes nothing about the published snapshot until a
// reload accepts it.
//
// A Config is safe for concurrent use. Current is lock-free and
// allocation-free; reloads are serialised; subscriber callbacks run outside
// every lock the package holds.
//
// The zero value is not usable. Construct one with New or Wrap, and Close
// it when the application is done with it.
type Config[T any] struct {
	// viper is the configuration engine. Whether it is reachable from
	// outside depends on how the Config was built; see Viper.
	viper *viper.Viper

	// current is the published snapshot. Readers load it without a lock,
	// which is why reads cost an atomic load and no allocation.
	current atomic.Pointer[T]

	// configFiles caches the files the published snapshot was read from,
	// in the order they were layered, so that Status and Watch need not
	// read Viper's fields while a reload writes them.
	configFiles atomic.Pointer[[]string]

	// fileStamps holds the size, mode and modification time of each of
	// those files at the moment it was read. Watch compares them against
	// what is on disk to close the gap between loading and watching
	// without republishing a configuration that never changed.
	fileStamps atomic.Pointer[map[string]fileStamp]

	// watching reports that a watch is established, rather than merely
	// claimed. Claiming happens first, so that a second Watch can be
	// refused; the flag is what Status reports, because "watching" has to
	// mean "changes are being observed" to be worth reporting at all.
	watching atomic.Bool

	// reloadSem serialises reload transactions. A one-slot channel rather
	// than a sync.Mutex because acquisition has to be selectable against
	// a caller's context: a Reload that cannot get in must be cancellable
	// rather than blocked.
	reloadSem chan struct{}

	// publishMu orders two events that must not interleave: the commit of
	// a new snapshot, and the transition to closing. Without it, a reload
	// that checked "not closed" before doing its work could publish a
	// generation after Close had already returned — a configuration
	// changing under an application that had shut its configuration down.
	//
	// The critical section is deliberately tiny. No file is read, nothing
	// is decoded, no validator runs and no callback fires while it is
	// held; it guards a handful of atomic stores and a lifecycle
	// transition, and nothing else.
	publishMu sync.Mutex

	// sealed records that the Viper instance is not reachable through
	// this Config. See NewSealed.
	sealed bool

	generation        atomic.Uint64
	successfulReloads atomic.Uint64
	failedReloads     atomic.Uint64

	// Times are held as Unix nanoseconds so that they can be read and
	// written atomically; zero means "never".
	lastSuccess atomic.Int64
	lastFailure atomic.Int64

	life *lifecycle.Lifecycle

	changeSubs registry[ChangeHandler[T]]
	errorSubs  registry[ErrorHandler]

	dispatcher *dispatch.Dispatcher

	opts options[T]
}

// Viper returns the underlying configuration engine, or nil if the Config
// was sealed.
//
// It is a method rather than a field so that the engine cannot be replaced
// from outside, and so that a sealed Config has an honest answer to give.
// Callers that may receive either shape should ask:
//
//	if v := cfg.Viper(); v != nil {
//	    v.SetDefault("server.port", 8080)
//	}
//
// The instance it returns is Viper's ordinary API, with Viper's ordinary
// concurrency properties — which is to say none. Viper does no internal
// locking and a reload writes its state, so it is safe to use before
// reloads can run and unsafe afterwards. Configure it during construction;
// read the application's configuration through Current.
func (c *Config[T]) Viper() *viper.Viper {
	if c.sealed {
		return nil
	}

	return c.viper
}

// Sealed reports whether the Viper instance is unreachable through this
// Config. It is the explicit form of a nil check on Viper.
func (c *Config[T]) Sealed() bool {
	return c.sealed
}

// Current returns the configuration the application should run on.
//
// It is the most recent snapshot that decoded and passed validation. After
// a successful New or Wrap it is never nil, including after a failed reload
// and after Close: a rejected candidate leaves the previous snapshot in
// place, and closing publishes nothing new.
//
// The returned value is an immutable snapshot by contract. The package
// cannot enforce that — Go has no immutable pointer — so it is stated
// instead: callers must not write through the pointer, nor through the
// maps, slices or pointers it reaches. Doing so mutates the configuration
// under every other goroutine holding the same snapshot.
//
// Read it once per unit of work and use that value throughout:
//
//	current := cfg.Current()
//
//	serve(current.Server.Host, current.Server.Port)
//
// rather than calling Current for each field, which can straddle a reload
// and mix two generations in one request.
func (c *Config[T]) Current() *T {
	return c.current.Load()
}

// Status returns counters, timestamps and lifecycle state — never
// configuration values, so it is safe to expose from a health or debug
// endpoint. See Status for the fields.
func (c *Config[T]) Status() Status {
	return Status{
		Generation:        c.generation.Load(),
		SuccessfulReloads: c.successfulReloads.Load(),
		FailedReloads:     c.failedReloads.Load(),
		LastSuccess:       loadTime(&c.lastSuccess),
		LastFailure:       loadTime(&c.lastFailure),
		Watching:          c.watching.Load(),
		Closed:            c.life.Closed(),
		ConfigFile:        c.configFileUsed(),
		ConfigFiles:       c.configFilesUsed(),
		DroppedEvents:     c.dispatcher.Dropped(),
	}
}

// ReloadCount returns the number of successful reloads, not counting the
// initial load. It is Status().SuccessfulReloads without the rest of the
// struct.
func (c *Config[T]) ReloadCount() uint64 {
	return c.successfulReloads.Load()
}

// Generation returns the number of snapshots published so far, counting the
// initial load. It advances only on a successful publication, which makes
// it the cheapest way to observe that a reload actually took effect.
func (c *Config[T]) Generation() uint64 {
	return c.generation.Load()
}

// Subscribe registers a handler for successful publications and returns a
// handle that cancels it.
//
//	sub := cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
//	    slog.Info("configuration reloaded", "generation", change.Generation)
//	})
//
//	defer sub.Unsubscribe()
//
// Handlers run on a single dispatcher goroutine, in publication order,
// outside every lock the package holds — so a handler may call back into
// the Config, including Reload, without deadlocking. Delivery is bounded
// and best-effort: see the delivery contract in doc.go. A handler that
// needs authoritative state should read Current rather than trust that it
// saw every event.
//
// Subscribing to a closed Config is accepted and delivers nothing.
func (c *Config[T]) Subscribe(handler ChangeHandler[T]) Subscription {
	if handler == nil {
		return newSubscription(nil)
	}

	id, ok := c.changeSubs.add(handler)
	if !ok {
		// Closed. The handler is not retained, so a subscription made
		// during shutdown cannot keep whatever it captured alive.
		return newSubscription(nil)
	}

	return newSubscription(func() { c.changeSubs.remove(id) })
}

// SubscribeErrors registers a handler for reload failures and returns a
// handle that cancels it.
//
//	cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
//	    slog.Error("configuration reload failed", "stage", e.Stage, "error", e.Err)
//	})
//
// A failure reported here means a candidate was rejected, not that the
// application lost its configuration: Current still returns the last good
// snapshot. The same execution rules as Subscribe apply.
func (c *Config[T]) SubscribeErrors(handler ErrorHandler) Subscription {
	if handler == nil {
		return newSubscription(nil)
	}

	id, ok := c.errorSubs.add(handler)
	if !ok {
		return newSubscription(nil)
	}

	return newSubscription(func() { c.errorSubs.remove(id) })
}

// Close releases the Config's resources: it stops the watcher, stops the
// dispatcher and drops every subscription.
//
// It is idempotent and safe from any goroutine. After Close, Reload and
// Watch return ErrClosed, while Current keeps returning the final snapshot
// — an application shutting down should not have its configuration pulled
// out from under the work still in flight.
//
// Close waits briefly for a subscriber callback that is already running.
// Queued notifications are abandoned rather than delivered, because
// shutdown must be bounded even when a subscriber is not. If a callback is
// still running when that patience runs out, Close returns an error saying
// so; the Config is closed either way.
func (c *Config[T]) Close() error {
	if !c.beginClose() {
		return nil
	}

	// From here no snapshot can be published: beginClose took the
	// publication gate to make the transition, so any reload still in
	// flight either committed before it or will find the Config closed
	// when it reaches its own commit.
	drained := c.dispatcher.Stop(closeTimeout)

	c.changeSubs.close()
	c.errorSubs.close()

	c.life.FinishClose()

	if c.opts.logger != nil {
		c.opts.logger.Debug(
			"dynamicconfig: closed",
			"generation", c.generation.Load(),
			"callbacks_drained", drained,
		)
	}

	if !drained {
		return errCallbacksOutstanding
	}

	return nil
}

// beginClose performs the lifecycle transition under the publication gate,
// so that closing and committing a snapshot cannot interleave: one of them
// happens first, and which one it was is unambiguous afterwards.
//
// It reports whether this call is the one that started closing.
func (c *Config[T]) beginClose() bool {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()

	return c.life.BeginClose()
}

// closed reports whether the Config is closing or closed.
func (c *Config[T]) closed() bool {
	return c.life.Closed()
}

// configFileUsed returns the primary file — the first one layered — or
// empty for a configuration that has none.
func (c *Config[T]) configFileUsed() string {
	files := c.configFilesUsed()
	if len(files) == 0 {
		return ""
	}

	return files[0]
}

// configFilesUsed returns a copy of the files the published snapshot was
// read from. A copy, because Status hands it to callers and a shared slice
// would be a shared mutable.
func (c *Config[T]) configFilesUsed() []string {
	stored := c.configFiles.Load()
	if stored == nil || len(*stored) == 0 {
		return nil
	}

	files := make([]string, len(*stored))
	copy(files, *stored)

	return files
}

func loadTime(v *atomic.Int64) time.Time {
	nanos := v.Load()
	if nanos == 0 {
		return time.Time{}
	}

	return time.Unix(0, nanos)
}

func storeTime(v *atomic.Int64, t time.Time) {
	v.Store(t.UnixNano())
}
