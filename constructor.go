package dynamicconfig

import (
	"fmt"

	"github.com/ctolon/dynamic-config-go/internal/dispatch"
	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
	"github.com/spf13/viper"
)

// New builds a Config with its own Viper instance and performs the initial
// load.
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
	return build(viper.New(), true, opts)
}

// Wrap adopts an existing Viper instance and performs the initial load.
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
// not take ownership of it: the caller must stop mutating it from other
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

func build[T any](v *viper.Viper, owned bool, opts []Option[T]) (*Config[T], error) {
	resolved := defaultOptions[T]()

	for i, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: option %d is nil", ErrInvalidOption, i)
		}

		if err := opt(&resolved); err != nil {
			return nil, err
		}
	}

	if resolved.configFile != "" {
		v.SetConfigFile(resolved.configFile)
	}

	for i, setup := range resolved.viperSetup {
		if err := setup(v); err != nil {
			return nil, fmt.Errorf("dynamicconfig: viper setup %d: %w", i, err)
		}
	}

	cfg := &Config[T]{
		Viper:     v,
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
			"owned_viper", owned,
		)
	}

	return cfg, nil
}
