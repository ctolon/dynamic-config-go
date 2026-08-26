package dynamicconfig

import (
	"sync/atomic"
	"time"

	"github.com/ctolon/dynamic-config-go/internal/dispatch"
	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
	"github.com/spf13/viper"
)

// Config publishes a validated, typed snapshot of T and replaces it
// atomically when the configuration on disk changes.
//
// The two halves of the type answer different questions, and the difference
// is the single most important thing to understand about the package:
//
//	cfg.Viper.GetInt("server.port")  // Viper's current, mutable state
//	cfg.Current().Server.Port        // the last snapshot that decoded and validated
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
	// Viper is the configuration engine, exposed on purpose rather than
	// hidden behind a second configuration API. Defaults, environment
	// binding, aliases, search paths and formats are Viper's job and stay
	// Viper's job.
	//
	// It is a field rather than an embedded type so that cfg.Current and
	// cfg.Viper.Get never collide, and so that the boundary between "what
	// Viper knows" and "what the application runs on" stays visible at
	// every call site.
	//
	// Reading or mutating it concurrently with a reload is not safe:
	// Viper does no internal locking, and a reload writes Viper's state.
	// Configure it before the first reload — or from a Viper setup
	// function — and read the application's configuration through
	// Current.
	Viper *viper.Viper

	// current is the published snapshot. Readers load it without a lock,
	// which is why reads cost an atomic load and no allocation.
	current atomic.Pointer[T]

	// configFile caches what Viper last read, so that Status and Watch
	// need not read Viper's fields while a reload writes them.
	configFile atomic.Pointer[string]

	// fileStamp is the size and modification time of the file at the last
	// successful read. Watch compares it against the file on disk to
	// close the gap between loading and watching without republishing a
	// configuration that never changed.
	fileStamp atomic.Pointer[fileStamp]

	// reloadSem serialises reload transactions. A one-slot channel rather
	// than a sync.Mutex because acquisition has to be selectable against
	// a caller's context: a Reload that cannot get in must be cancellable
	// rather than blocked.
	reloadSem chan struct{}

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
		Watching:          c.life.Watching(),
		Closed:            c.life.Closed(),
		ConfigFile:        c.configFileUsed(),
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

	id := c.changeSubs.add(handler)

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

	id := c.errorSubs.add(handler)

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
	if !c.life.BeginClose() {
		return nil
	}

	drained := c.dispatcher.Stop(closeTimeout)

	c.changeSubs.clear()
	c.errorSubs.clear()

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

// closed reports whether the Config is closing or closed.
func (c *Config[T]) closed() bool {
	return c.life.Closed()
}

func (c *Config[T]) configFileUsed() string {
	if p := c.configFile.Load(); p != nil {
		return *p
	}

	return ""
}

func (c *Config[T]) setConfigFileUsed(path string) {
	c.configFile.Store(&path)
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
