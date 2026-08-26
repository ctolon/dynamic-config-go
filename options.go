package dynamicconfig

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/viper"
)

// Defaults for the tunable options. They favour predictable production
// behaviour over configurability; the proposal's reasoning is preserved in
// docs/design.md.
const (
	// DefaultDebounce is how long a burst of filesystem events must go
	// quiet before a reload runs. Long enough to fold an editor's
	// write/chmod/rename sequence into one reload, short enough that a
	// deliberate change appears to take effect immediately.
	DefaultDebounce = 200 * time.Millisecond

	// DefaultEventBuffer is the depth of the subscriber dispatch queue.
	// Small on purpose: the queue exists to decouple a slow subscriber
	// from publication, not to archive events.
	DefaultEventBuffer = 16

	// closeTimeout bounds how long Close waits for a subscriber callback
	// that is already running. Shutdown has to be deterministic even when
	// a subscriber is not.
	closeTimeout = 5 * time.Second
)

// configFile is one file the configuration is read from, in the order the
// options named it.
type configFile struct {
	path     string
	optional bool
}

// options is the resolved configuration of a Config. It is unexported:
// every field is set through an Option, so the set of legal states is the
// set the option functions can produce.
type options[T any] struct {
	configFiles []configFile

	allowMissingFile bool

	validator Validator[T]

	debounce time.Duration

	eventBuffer int

	logger *slog.Logger

	decodeOptions []viper.DecoderConfigOption

	viperSetup []func(*viper.Viper) error
}

func defaultOptions[T any]() options[T] {
	return options[T]{
		debounce:    DefaultDebounce,
		eventBuffer: DefaultEventBuffer,
	}
}

// Option configures a Config at construction.
//
// Options are validated when they are applied, not when they are
// constructed, so a mistake surfaces as an error from New or Wrap rather
// than as a silently normalised value. Every such error wraps
// ErrInvalidOption.
type Option[T any] func(*options[T]) error

// WithConfigFile adds a configuration file to read.
//
// It is the equivalent of viper.SetConfigFile: the path is used exactly as
// given, with no search and no name or extension inference.
//
// Calling it more than once builds a layered configuration from several
// files, read in the order the options were given, with later files
// overriding keys from earlier ones — the same rule Viper's own merge
// follows:
//
//	dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
//	dynamicconfig.WithOptionalConfigFile[AppConfig]("/etc/myapp/secrets.yaml"),
//
// Every file is required unless it was added with WithOptionalConfigFile. A
// reload reads all of them, so one transaction publishes one snapshot
// however many files it came from, and a file that becomes unreadable
// rejects the whole candidate rather than publishing half of it.
//
// Watch watches every file that was read, each in its own directory.
//
// For anything more elaborate — a search path, a config name, an explicit
// format for an extensionless file — use WithViperSetup, or build the Viper
// instance yourself and pass it to Wrap.
func WithConfigFile[T any](path string) Option[T] {
	return func(o *options[T]) error {
		if path == "" {
			return fmt.Errorf("%w: config file path is empty", ErrInvalidOption)
		}

		o.configFiles = append(o.configFiles, configFile{path: path})

		return nil
	}
}

// WithOptionalConfigFile adds a configuration file that may or may not
// exist.
//
// It layers exactly like WithConfigFile — later files override earlier keys
// — but a missing one is skipped instead of failing the read. This is the
// shape of a secret file mounted only in some environments, or a local
// override a developer may not have:
//
//	dynamicconfig.WithConfigFile[AppConfig]("config.yaml"),
//	dynamicconfig.WithOptionalConfigFile[AppConfig]("config.local.yaml"),
//
// Missing means absent. A file that exists and cannot be read, or cannot be
// parsed, is still a failure — the point is to make absence legal, not to
// paper over a broken file.
func WithOptionalConfigFile[T any](path string) Option[T] {
	return func(o *options[T]) error {
		if path == "" {
			return fmt.Errorf("%w: config file path is empty", ErrInvalidOption)
		}

		o.configFiles = append(o.configFiles, configFile{path: path, optional: true})

		return nil
	}
}

// WithValidator sets the function that decides whether a decoded
// configuration may be published.
//
// Without a validator the package still guarantees atomic publication and
// last-known-good behaviour on a decode failure — but "good" then means
// only "parsed". A validator is what makes the guarantee worth something.
func WithValidator[T any](validator Validator[T]) Option[T] {
	return func(o *options[T]) error {
		if validator == nil {
			return fmt.Errorf("%w: validator is nil", ErrInvalidOption)
		}

		o.validator = validator

		return nil
	}
}

// WithDebounce sets how long filesystem events must go quiet before a
// reload runs. Zero disables debouncing, which is useful in tests that want
// a reload per event; negative is an error.
func WithDebounce[T any](d time.Duration) Option[T] {
	return func(o *options[T]) error {
		if d < 0 {
			return fmt.Errorf("%w: debounce %s is negative", ErrInvalidOption, d)
		}

		o.debounce = d

		return nil
	}
}

// WithEventBuffer sets the depth of the subscriber dispatch queue. It must
// be at least one.
//
// Raising it lets a bursty subscriber fall further behind before events are
// dropped. It does not make delivery reliable — nothing does; see the
// delivery contract in doc.go.
func WithEventBuffer[T any](size int) Option[T] {
	return func(o *options[T]) error {
		if size < 1 {
			return fmt.Errorf("%w: event buffer %d is below 1", ErrInvalidOption, size)
		}

		o.eventBuffer = size

		return nil
	}
}

// WithLogger attaches a *slog.Logger.
//
// Logging is optional and log/slog is in the standard library, so this
// costs no dependency. The package logs stages, counters and errors — never
// configuration values, and never the contents of a Change.
func WithLogger[T any](logger *slog.Logger) Option[T] {
	return func(o *options[T]) error {
		if logger == nil {
			return fmt.Errorf("%w: logger is nil", ErrInvalidOption)
		}

		o.logger = logger

		return nil
	}
}

// WithDecodeOption passes Viper decoder options through to every Unmarshal
// the package performs, so that the decode used at reload is the decode the
// application expects.
//
//	dynamicconfig.WithDecodeOption[AppConfig](
//	    viper.DecodeHook(mapstructure.StringToTimeDurationHookFunc()),
//	)
//
// Calls accumulate rather than replace.
func WithDecodeOption[T any](opts ...viper.DecoderConfigOption) Option[T] {
	return func(o *options[T]) error {
		for _, opt := range opts {
			if opt == nil {
				return fmt.Errorf("%w: nil decode option", ErrInvalidOption)
			}
		}

		o.decodeOptions = append(o.decodeOptions, opts...)

		return nil
	}
}

// WithAllowMissingFile decides what it means for Viper to find no
// configuration file at all.
//
// It covers the case where nothing was named and nothing was found: no
// WithConfigFile, and a search path that turned up empty. By default that
// is fatal — a service that cannot find its configuration should not start,
// and startup is the cheapest moment to find out. Allow it when the
// configuration legitimately comes from defaults and the environment.
//
// For a *named* file that may be absent, use WithOptionalConfigFile
// instead; it says which file is optional rather than making absence legal
// in general.
//
// It applies while no file has ever been read. Once a snapshot has been
// published from a file, that file disappearing is always a reload failure
// and never clears the published snapshot.
func WithAllowMissingFile[T any](allow bool) Option[T] {
	return func(o *options[T]) error {
		o.allowMissingFile = allow

		return nil
	}
}

// WithViperSetup runs fn against the Viper instance before the initial
// load.
//
// This is the escape hatch that keeps the option list short. Viper already
// has an API for defaults, environment binding, search paths, aliases and
// formats; mirroring it here would double the surface for nothing:
//
//	dynamicconfig.WithViperSetup[AppConfig](func(v *viper.Viper) error {
//	    v.SetConfigName("app")
//	    v.AddConfigPath("/etc/myapp")
//	    v.SetDefault("server.port", 8080)
//	    v.SetEnvPrefix("MYAPP")
//	    v.AutomaticEnv()
//
//	    return nil
//	})
//
// Calls accumulate and run in order. An error returned by fn fails
// construction.
func WithViperSetup[T any](fn func(*viper.Viper) error) Option[T] {
	return func(o *options[T]) error {
		if fn == nil {
			return fmt.Errorf("%w: viper setup function is nil", ErrInvalidOption)
		}

		o.viperSetup = append(o.viperSetup, fn)

		return nil
	}
}
