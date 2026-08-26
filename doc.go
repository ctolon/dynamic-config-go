// Package dynamicconfig turns Viper configuration into validated typed
// snapshots that can be replaced atomically at runtime, without
// interrupting concurrent readers and without restarting the process.
//
// Viper keeps its job. Files, formats, defaults, environment variables,
// aliases and search paths are Viper's. This package owns what a
// long-running service has to build on top of Viper anyway: decode into a
// typed struct, validate, publish atomically, and never replace a good
// configuration with a bad one.
//
//	Viper state ──► decode into T ──► validate ──► publish ──► Current()
//	                                      │
//	                                   invalid
//	                                      │
//	                                      └──► rejected, previous snapshot stays
//
// # Getting started
//
//	type AppConfig struct {
//	    Server ServerConfig `mapstructure:"server"`
//	}
//
//	type ServerConfig struct {
//	    Host string `mapstructure:"host"`
//	    Port int    `mapstructure:"port"`
//	}
//
//	cfg, err := dynamicconfig.New[AppConfig](
//	    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
//	    dynamicconfig.WithValidator(func(c *AppConfig) error {
//	        if c.Server.Port < 1 || c.Server.Port > 65535 {
//	            return fmt.Errorf("server.port %d out of range", c.Server.Port)
//	        }
//
//	        return nil
//	    }),
//	)
//	if err != nil {
//	    return fmt.Errorf("initialize configuration: %w", err)
//	}
//
//	defer cfg.Close()
//
//	go cfg.Watch(ctx)
//
// From then on, every unit of work reads one snapshot:
//
//	current := cfg.Current()
//
//	serve(current.Server.Host, current.Server.Port)
//
// # Two shapes: open and sealed
//
// A Config either lets you reach the Viper instance behind it or it does
// not, and that is a construction-time choice:
//
//	                 │ engine reachable      │ engine sealed
//	─────────────────┼───────────────────────┼─────────────────────
//	built for you    │ New                   │ NewSealed
//	your instance    │ Wrap                  │ WrapSealed
//
// An open Config hands the engine back through [Config.Viper], so
// everything Viper does well stays available and an existing Viper codebase
// can adopt this package without rewriting anything.
//
// A sealed Config returns nil from [Config.Viper]. The engine cannot be
// read, mutated or replaced through the Config, so the configuration has
// exactly one public interface — Current, Reload, Watch and the rest of
// this package. Sealing removes two classes of problem outright: a second
// configuration API spreading through an application as
// cfg.Viper().GetString calls, and a goroutine reading the engine while a
// reload writes it, which Viper does not synchronise and this package
// cannot synchronise on its behalf.
//
// Neither shape is the lesser one. Sealing suits a configuration that will
// be handed to code that should depend on the configuration rather than on
// the machinery behind it; leaving it open suits an application that
// genuinely still uses Viper.
//
// # Current and the engine are not the same thing
//
// In an open Config:
//
//	cfg.Viper().GetInt("server.port")  // Viper's current, mutable state
//	cfg.Current().Server.Port          // the last snapshot that decoded and validated
//
// They can disagree. After
//
//	cfg.Viper().Set("server.port", -1)
//
// Viper reports -1 immediately, while Current keeps returning the last good
// port until a reload accepts a candidate. Setting a value on Viper does
// not publish it; a reload does:
//
//	cfg.Viper().Set("feature.enabled", true)
//
//	if err := cfg.Reload(ctx); err != nil {
//	    return err
//	}
//
// The same is true of the environment. Viper reads environment variables
// when it is asked, so a variable that changes in the running process is
// picked up by the next reload — and nothing triggers that reload, because
// only files produce events. Call Reload when the environment changes
// underneath you.
//
// # Guarantees
//
// After a successful construction:
//
//   - Current is never nil, and construction publishes exactly generation 1.
//   - Only configurations that were read, decoded and validated are ever
//     published.
//   - A failed reload never changes what Current returns — not to nil, not
//     to a half-decoded value, not to the rejected candidate.
//   - Readers see snapshot N or snapshot N+1, never a mixture.
//   - Generation advances exactly once per publication and never decreases.
//   - Reload transactions are serialised; readers never take a lock.
//   - Once Close begins, nothing is ever published again.
//   - Current stays readable, and frozen, after Close.
//   - Subscriber callbacks run outside every lock the package holds, so a
//     handler may call back into the Config.
//   - A panicking subscriber costs its own callback and nothing else.
//   - A panicking validator rejects its candidate and nothing else.
//   - Close is idempotent, and every internal queue is bounded.
//   - No configuration value is ever logged by this package.
//
// # The snapshot contract
//
// Current returns a *T that callers must treat as immutable. Go cannot
// enforce that, so it is a contract rather than a guarantee: writing
// through the pointer — or through the maps, slices and pointers it reaches
// — mutates the configuration under every other goroutine holding the same
// snapshot, with no synchronisation whatsoever.
//
//	cfg.Current().Server.Port = 8081       // unsupported
//	cfg.Current().Features["beta"] = true  // unsupported, and a data race
//
// Returning a copy instead would not fix this, since a shallow copy shares
// the same maps and slices, and deep-copying on every read would destroy
// the property that makes Current worth calling per request: it costs an
// atomic load and allocates nothing. Applications that need a mutable value
// should copy the parts they intend to change.
//
// A new snapshot also does not reconfigure anything the application already
// built from the old one. A connection pool, an HTTP client, a TLS
// configuration or a worker pool goes on using whatever it was constructed
// with; rebuilding it is the application's job, usually from a subscriber.
//
// # Validation
//
// The validator decides what may be published, and it is ordinary Go — no
// framework, no struct tags, no dependency. It runs synchronously, inside
// the reload transaction, on the goroutine that called Reload or on the
// watcher's. It should be quick, local and deterministic: network calls
// belong outside it, because they make reload latency unbounded and tie
// configuration acceptance to somebody else's uptime.
//
// A validator must not call back into the Config it validates for — Reload
// and Close both wait on the transaction it is running inside. A panic in a
// validator is recovered and turned into a rejected candidate.
//
// # Delivery of events
//
// Publication is reliable. Delivery to subscribers is best-effort and
// bounded, and that asymmetry is deliberate: a subscriber must never be
// able to delay or block a configuration change.
//
// Handlers run on one dispatcher goroutine, one at a time, in publication
// order. The queue between publication and delivery has a fixed depth
// ([WithEventBuffer]); when a slow subscriber fills it, the oldest pending
// notification is dropped and Status().DroppedEvents advances. For
// configuration, the newest state is the one worth keeping.
//
// A subscriber that needs authoritative state must read Current rather than
// assume it saw every event. Subscription mechanics are for reacting to
// change — reopening a pool, re-resolving an endpoint — not for
// reconstructing the configuration's history.
//
// # Shutdown
//
// Close is idempotent and safe from any goroutine. It freezes publication
// first, so a reload already in flight either committed before the close or
// finds the Config closed at its commit and returns [ErrClosed]. It then
// stops the watcher, stops the dispatcher — waiting briefly for a callback
// that is already running, abandoning what is merely queued — and drops
// every subscription without retaining the handlers.
//
// After Close, Current keeps returning the final snapshot for as long as
// anything holds the Config, and Reload and Watch return [ErrClosed].
//
// # Concurrency
//
// Safe concurrently: Current, Status, Generation, ReloadCount, Reload,
// Subscribe, SubscribeErrors, Unsubscribe and Close, in any combination,
// from any number of goroutines. One watcher may run at a time; a second
// Watch returns [ErrAlreadyWatching].
//
// Not safe: mutating a value returned by Current, and using an open
// Config's Viper instance from another goroutine while reloads can run.
// Viper does no internal locking and a reload writes Viper's state, so a
// concurrent Get is a data race that this package cannot prevent on Viper's
// behalf. Configure the engine during construction — through options,
// [WithViperSetup], or before [Wrap] — and read the application's
// configuration through Current. Sealing makes that rule structural rather
// than advisory.
//
// # Secrets
//
// A configuration struct is exactly the kind of thing that holds a database
// password, so the package treats values as radioactive. Errors name
// stages, paths and types, never values; [Change] has no String or JSON
// method; [Status] carries counters and timestamps only, so it is safe to
// expose from a health endpoint.
//
// What the package will not do, an application can still do to itself:
//
//	slog.Info("config", "value", cfg.Current())            // don't
//	return fmt.Errorf("bad password %q", c.Password)       // don't: validator
//	                                                       // errors are logged
//
// # Filesystems
//
// Watching is fsnotify's, and fsnotify's guarantees are the operating
// system's. Local filesystems on Linux, macOS and Windows are tested, as
// are Kubernetes projected volumes on Linux. Network filesystems — NFS,
// SMB, FUSE — often deliver no events at all, and no library can invent
// them; an application on one should reload on a signal or a timer instead.
// See docs/compatibility.md.
package dynamicconfig
