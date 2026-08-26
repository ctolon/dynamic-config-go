package dynamicconfig

import (
	"fmt"

	"github.com/ctolon/dynamic-config-go/internal/dispatch"
	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
	"github.com/spf13/viper"
)

// There are four constructors, along two axes: who owns the Viper instance,
// and whether the Config lets anything reach it.
//
//	                 │ engine reachable      │ engine sealed
//	─────────────────┼───────────────────────┼─────────────────────
//	built for you    │ New                   │ NewSealed
//	your instance    │ Wrap                  │ WrapSealed
//
// Reachable is the pragmatic shape: cfg.Viper() hands back the engine, and
// everything Viper does well stays available. Sealed is the encapsulated
// shape: cfg.Viper() is nil, the engine cannot be read, mutated or replaced
// through the Config, and the configuration has exactly one public
// interface — Current, Reload, Watch and the rest of this package.
//
// Neither is a lesser option. Which one fits depends on whether the Config
// is going to be passed around a codebase that should not be able to reach
// past it.

// New builds a Config with its own Viper instance, reachable through Viper,
// and performs the initial load.
//
//	cfg, err := dynamicconfig.New[AppConfig](
//	    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
//	    dynamicconfig.WithValidator(validateConfig),
//	)
//	if err != nil {
//	    return fmt.Errorf("initialize configuration: %w", err)
//	}
//
//	defer cfg.Close()
//
// Construction fails if the file cannot be read, cannot be decoded into T,
// or does not validate. That is the intended behaviour: a service should
// not start on configuration it could not understand, and startup is the
// cheapest moment to find out. A configuration that legitimately has no
// file can pass WithAllowMissingFile.
//
// A successful New returns a Config whose Current is already non-nil, so
// application code never has to consider a nil configuration.
//
// New does not start a watcher. Filesystem watching is a goroutine with a
// lifetime, and the application owns it:
//
//	go cfg.Watch(ctx)
func New[T any](opts ...Option[T]) (*Config[T], error) {
	return build(viper.New(), false, opts)
}

// NewSealed builds a Config with its own Viper instance and keeps it
// unreachable: Viper returns nil, and nothing holding the Config can read,
// mutate or replace the engine underneath it.
//
//	cfg, err := dynamicconfig.NewSealed[AppConfig](
//	    dynamicconfig.WithViperSetup[AppConfig](func(v *viper.Viper) error {
//	        v.SetConfigFile("/etc/myapp/config.yaml")
//	        v.SetEnvPrefix("MYAPP")
//	        v.AutomaticEnv()
//
//	        return nil
//	    }),
//	    dynamicconfig.WithValidator(validateConfig),
//	)
//
// Everything else behaves identically to New. The engine is still
// configurable — during construction, through the options and
// WithViperSetup, which is the only moment at which configuring it is safe
// anyway.
//
// Seal a Config when it will be handed to code that should depend on the
// configuration rather than on the machinery behind it. It removes two
// classes of problem outright: a second configuration API spreading through
// an application as cfg.Viper().GetString calls, and a goroutine reading
// the engine while a reload writes it, which Viper does not synchronise and
// this package cannot synchronise on its behalf.
func NewSealed[T any](opts ...Option[T]) (*Config[T], error) {
	return build(viper.New(), true, opts)
}

// Wrap adopts an existing Viper instance, keeps it reachable through Viper,
// and performs the initial load.
//
// This is the migration path for an application that already configures
// Viper — defaults, environment prefix, search paths, aliases — and wants
// typed snapshots and safe reloads without rewriting any of it:
//
//	v := viper.New()
//	v.SetConfigFile("config.yaml")
//	v.SetEnvPrefix("MYAPP")
//	v.AutomaticEnv()
//
//	cfg, err := dynamicconfig.Wrap[AppConfig](v, dynamicconfig.WithValidator(validate))
//
// The Config takes over reading and decoding through that instance. It does
// not take ownership of it: the caller must stop using it from other
// goroutines once reloads can run, because Viper does no locking of its
// own.
//
// Wrap re-reads the configuration even if the caller has already called
// ReadInConfig, so that the published snapshot and Viper's state start out
// agreeing.
func Wrap[T any](v *viper.Viper, opts ...Option[T]) (*Config[T], error) {
	if v == nil {
		return nil, fmt.Errorf("%w: viper instance is nil", ErrInvalidOption)
	}

	return build(v, false, opts)
}

// WrapSealed adopts an existing Viper instance and seals it: the Config
// gives nothing back, and Viper returns nil.
//
// The caller obviously still holds the instance it passed in — no library
// can take that away. What sealing does is stop the Config from being a
// route to it, which is what matters at a package boundary:
//
//	func Load() (*dynamicconfig.Config[AppConfig], error) {
//	    v := viper.New()
//	    v.SetConfigFile("/etc/myapp/config.yaml")
//	    v.SetEnvPrefix("MYAPP")
//	    v.AutomaticEnv()
//
//	    return dynamicconfig.WrapSealed[AppConfig](v, dynamicconfig.WithValidator(validate))
//	}
//
// The local v goes out of scope, and every caller of Load receives a
// configuration with one public interface and no way behind it.
func WrapSealed[T any](v *viper.Viper, opts ...Option[T]) (*Config[T], error) {
	if v == nil {
		return nil, fmt.Errorf("%w: viper instance is nil", ErrInvalidOption)
	}

	return build(v, true, opts)
}

func build[T any](v *viper.Viper, sealed bool, opts []Option[T]) (*Config[T], error) {
	resolved := defaultOptions[T]()

	for i, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: option %d is nil", ErrInvalidOption, i)
		}

		if err := opt(&resolved); err != nil {
			return nil, err
		}
	}

	for i, setup := range resolved.viperSetup {
		if err := setup(v); err != nil {
			return nil, fmt.Errorf("dynamicconfig: viper setup %d: %w", i, err)
		}
	}

	cfg := &Config[T]{
		viper:     v,
		sealed:    sealed,
		reloadSem: make(chan struct{}, 1),
		life:      lifecycle.New(),
		opts:      resolved,
	}

	cfg.dispatcher = dispatch.New(resolved.eventBuffer, cfg.onCallbackPanic)

	// The initial load runs through the same transaction as every later
	// reload — one implementation, so the guarantees cannot drift apart
	// between the path that starts the process and the path that keeps it
	// running.
	if err := cfg.initialLoad(); err != nil {
		return nil, err
	}

	cfg.dispatcher.Start()

	cfg.life.Ready()

	if resolved.logger != nil {
		resolved.logger.Debug(
			"dynamicconfig: configuration loaded",
			"config_file", cfg.configFileUsed(),
			"generation", cfg.generation.Load(),
			"sealed", sealed,
		)
	}

	return cfg, nil
}
