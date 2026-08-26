// Package dynamicconfig turns Viper configuration into validated typed
// snapshots that can be replaced atomically at runtime, without
// interrupting concurrent readers and without restarting the process.
//
// Viper keeps its job. Files, formats, defaults, environment variables,
// aliases and search paths are Viper's, and stay reachable through the
// exposed Viper field. This package owns what a long-running service has to
// build on top of Viper anyway: decode into a typed struct, validate,
// publish atomically, and never replace a good configuration with a bad
// one.
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
// # Current and Viper are not the same thing
//
// This is the distinction to internalise before anything else:
//
//	cfg.Viper.GetInt("server.port")  // Viper's current, mutable state
//	cfg.Current().Server.Port        // the last snapshot that decoded and validated
//
// They can disagree. After
//
//	cfg.Viper.Set("server.port", -1)
//
// Viper reports -1 immediately, while Current keeps returning the last good
// port until a reload accepts a candidate. Setting a value on Viper does
// not publish it; a reload does:
//
//	cfg.Viper.Set("feature.enabled", true)
//
//	if err := cfg.Reload(ctx); err != nil {
//	    return err
//	}
//
// # Guarantees
//
// After a successful New or Wrap:
//
//   - Current is never nil.
//   - Only configurations that decoded and validated are ever published.
//   - A failed reload never changes what Current returns — not to nil, not
//     to a half-decoded value, not to the rejected candidate.
//   - Readers see snapshot N or snapshot N+1, never a mixture.
//   - Reload transactions are serialised; readers never take a lock.
//   - Subscriber callbacks run outside every lock the package holds, so a
//     handler may call back into the Config.
//   - A panicking subscriber costs its own callback and nothing else.
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
// # Delivery of events
//
// Publication is reliable. Delivery to subscribers is best-effort and
// bounded, and that asymmetry is deliberate: a subscriber must never be
// able to delay or block a configuration change.
//
// Handlers run on one dispatcher goroutine, one at a time, in publication
// order. The queue between publication and delivery has a fixed depth
// (WithEventBuffer); when a slow subscriber fills it, the oldest pending
// notification is dropped and Status().DroppedEvents advances. For
// configuration, the newest state is the one worth keeping.
//
// A subscriber that needs authoritative state must read Current rather than
// assume it saw every event. Subscription mechanics are for reacting to
// change — reopening a pool, re-resolving an endpoint — not for
// reconstructing the configuration's history.
//
// # Concurrency
//
// Safe concurrently: Current, Status, Generation, ReloadCount, Reload,
// Subscribe, SubscribeErrors, Unsubscribe and Close, in any combination,
// from any number of goroutines. One watcher may run at a time; a second
// Watch returns ErrAlreadyWatching.
//
// Not safe: mutating a value returned by Current, and using cfg.Viper from
// another goroutine while reloads can run. Viper does no internal locking
// and a reload writes Viper's state, so a concurrent Get is a data race
// that this package cannot prevent on Viper's behalf. Configure Viper
// before the first reload — through options, WithViperSetup, or before
// Wrap — and read the application's configuration through Current.
//
// # Secrets
//
// A configuration struct is exactly the kind of thing that holds a database
// password, so the package treats values as radioactive. Errors name
// stages, paths and types, never values; Change has no String or JSON
// method; Status carries counters and timestamps only, so it is safe to
// expose from a health endpoint. What the package will not do, an
// application can still do to itself:
//
//	slog.Info("config", "value", cfg.Current())  // don't
//
// # Kubernetes
//
// A ConfigMap or Secret mounted as a projected volume is updated by
// swapping a ..data symlink, not by writing to the file. The watcher
// handles that case explicitly, along with the rename-into-place that
// editors and deployment tools use, and with deletion followed by
// re-creation. See docs/kubernetes.md.
package dynamicconfig
