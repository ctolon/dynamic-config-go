package dynamicconfig_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
	"github.com/spf13/viper"
)

// ExampleConfig is the shape an application decodes into. The mapstructure
// tags are Viper's.
type ExampleConfig struct {
	Server ExampleServer `mapstructure:"server"`
}

// ExampleServer is where the process listens.
type ExampleServer struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

func exampleValidate(c *ExampleConfig) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is outside 1-65535", c.Server.Port)
	}

	return nil
}

// writeExampleFile writes a configuration file into a temporary directory
// and returns its path along with a cleanup function.
func writeExampleFile(contents string) (path string, cleanup func()) {
	dir, err := os.MkdirTemp("", "dynamicconfig-example")
	if err != nil {
		log.Fatal(err)
	}

	path = filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		log.Fatal(err)
	}

	return path, func() { _ = os.RemoveAll(dir) }
}

// Example shows the whole of the ordinary usage: load, validate, read.
func Example() {
	path, cleanup := writeExampleFile("server:\n  host: localhost\n  port: 8080\n")

	defer cleanup()

	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithConfigFile[ExampleConfig](path),
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		// A service that cannot understand its configuration should not
		// start.
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	// One snapshot, used throughout the unit of work.
	current := cfg.Current()

	fmt.Printf("%s:%d\n", current.Server.Host, current.Server.Port)

	// Output:
	// localhost:8080
}

// ExampleConfig_Reload shows the guarantee that matters most: a
// configuration that does not validate is rejected, and the running one is
// untouched.
func ExampleConfig_Reload() {
	path, cleanup := writeExampleFile("server:\n  host: localhost\n  port: 8080\n")

	defer cleanup()

	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithConfigFile[ExampleConfig](path),
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = cfg.Close() }()

	// Somebody writes a port that cannot exist.
	if err := os.WriteFile(path, []byte("server:\n  port: 70000\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	if err := cfg.Reload(context.Background()); err != nil {
		fmt.Println("rejected")
	}

	fmt.Printf("still serving port %d, generation %d\n",
		cfg.Current().Server.Port, cfg.Generation())

	// The file is repaired, and the next reload publishes.
	if err := os.WriteFile(path, []byte("server:\n  host: localhost\n  port: 9090\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	if err := cfg.Reload(context.Background()); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("now serving port %d, generation %d\n",
		cfg.Current().Server.Port, cfg.Generation())

	// Output:
	// rejected
	// still serving port 8080, generation 1
	// now serving port 9090, generation 2
}

// ExampleWrap adopts a Viper instance an application has already
// configured, which is the migration path from plain Viper.
func ExampleWrap() {
	path, cleanup := writeExampleFile("server:\n  host: localhost\n")

	defer cleanup()

	v := viper.New()

	v.SetConfigFile(path)
	v.SetDefault("server.port", 8080)
	v.SetEnvPrefix("MYAPP")
	v.AutomaticEnv()

	cfg, err := dynamicconfig.Wrap[ExampleConfig](v,
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = cfg.Close() }()

	// Everything already configured on that instance keeps working — the
	// default below came from Viper, not from the file.
	fmt.Printf("%s:%d\n", cfg.Current().Server.Host, cfg.Current().Server.Port)

	// Output:
	// localhost:8080
}

// ExampleConfig_Subscribe reacts to a change. Handlers run in publication
// order, on a goroutine of the package's own, outside every lock it holds.
func ExampleConfig_Subscribe() {
	path, cleanup := writeExampleFile("server:\n  host: localhost\n  port: 8080\n")

	defer cleanup()

	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithConfigFile[ExampleConfig](path),
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = cfg.Close() }()

	reloaded := make(chan uint64, 1)

	sub := cfg.Subscribe(func(change dynamicconfig.Change[ExampleConfig]) {
		// Log the generation, never the values: a configuration struct
		// is the kind of thing that grows a password field later.
		reloaded <- change.Generation
	})

	defer sub.Unsubscribe()

	if err := os.WriteFile(path, []byte("server:\n  host: localhost\n  port: 9090\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	if err := cfg.Reload(context.Background()); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("reloaded to generation %d\n", <-reloaded)

	// Output:
	// reloaded to generation 2
}

// ExampleConfig_SubscribeErrors observes rejected reloads. A failure here
// means a candidate was refused, never that the configuration was lost.
func ExampleConfig_SubscribeErrors() {
	path, cleanup := writeExampleFile("server:\n  host: localhost\n  port: 8080\n")

	defer cleanup()

	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithConfigFile[ExampleConfig](path),
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = cfg.Close() }()

	failures := make(chan dynamicconfig.ReloadStage, 1)

	sub := cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
		failures <- e.Stage
	})

	defer sub.Unsubscribe()

	if err := os.WriteFile(path, []byte("server:\n  port: 70000\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	_ = cfg.Reload(context.Background())

	fmt.Printf("rejected at the %s stage, still on generation %d\n",
		<-failures, cfg.Generation())

	// Output:
	// rejected at the validation stage, still on generation 1
}

// ExampleConfig_Status exposes the configuration's health without exposing
// the configuration: counters, timestamps and state, never values.
func ExampleConfig_Status() {
	path, cleanup := writeExampleFile("server:\n  host: localhost\n  port: 8080\n")

	defer cleanup()

	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithConfigFile[ExampleConfig](path),
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = cfg.Close() }()

	status := cfg.Status()

	fmt.Printf("generation=%d successful=%d failed=%d watching=%v closed=%v\n",
		status.Generation,
		status.SuccessfulReloads,
		status.FailedReloads,
		status.Watching,
		status.Closed,
	)

	// Output:
	// generation=1 successful=0 failed=0 watching=false closed=false
}

// ExampleConfig_Watch is the canonical shape of a long-running service:
// load, watch on a goroutine the application owns, and read one snapshot
// per unit of work.
func ExampleConfig_Watch() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	defer stop()

	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithConfigFile[ExampleConfig]("/etc/myapp/config.yaml"),
		dynamicconfig.WithValidator(exampleValidate),
		dynamicconfig.WithDebounce[ExampleConfig](200*time.Millisecond),
	)
	if err != nil {
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	// Watch blocks, so the goroutine's lifetime belongs to the
	// application and is visible here.
	go func() {
		if err := cfg.Watch(ctx); err != nil &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, dynamicconfig.ErrClosed) {
			log.Printf("configuration watcher stopped: %v", err)
		}
	}()

	<-ctx.Done()
}

// ExampleWithViperSetup keeps the option list short by leaving everything
// Viper already does well to Viper.
func ExampleWithViperSetup() {
	cfg, err := dynamicconfig.New[ExampleConfig](
		dynamicconfig.WithViperSetup[ExampleConfig](func(v *viper.Viper) error {
			v.SetConfigName("config")
			v.SetConfigType("yaml")
			v.AddConfigPath("/etc/myapp")

			v.SetDefault("server.host", "0.0.0.0")
			v.SetDefault("server.port", 8080)

			v.SetEnvPrefix("MYAPP")
			v.AutomaticEnv()

			return nil
		}),

		// There may be no file at all; defaults and the environment are
		// enough.
		dynamicconfig.WithAllowMissingFile[ExampleConfig](true),
		dynamicconfig.WithValidator(exampleValidate),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = cfg.Close() }()

	fmt.Printf("%s:%d\n", cfg.Current().Server.Host, cfg.Current().Server.Port)

	// Output:
	// 0.0.0.0:8080
}
