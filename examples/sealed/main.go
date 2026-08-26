// Command sealed builds a configuration that nothing can reach behind.
//
//	go run . -config config.yaml
//
// The pattern is the interesting part: a package exposes a loader, the
// Viper instance never escapes it, and every caller downstream gets a
// configuration with exactly one public interface.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
	"github.com/spf13/viper"
)

// AppConfig is the shape the application expects.
type AppConfig struct {
	Server   ServerConfig    `mapstructure:"server"`
	Features map[string]bool `mapstructure:"features"`
}

// ServerConfig is where the process listens.
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

func validate(c *AppConfig) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is outside 1-65535", c.Server.Port)
	}

	if c.Server.Host == "" {
		return errors.New("server.host is empty")
	}

	return nil
}

// Load builds the configuration and seals it.
//
// Everything Viper needs to know is said here, at construction, which is
// the only moment at which configuring it is safe anyway: reloads have not
// started, and no other goroutine can be reading it. The local v then goes
// out of scope, and WrapSealed makes sure the returned Config is not a
// second route to it.
func Load(path string) (*dynamicconfig.Config[AppConfig], error) {
	v := viper.New()

	v.SetConfigFile(path)

	v.SetDefault("server.host", "127.0.0.1")
	v.SetDefault("server.port", 8080)

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	return dynamicconfig.WrapSealed[AppConfig](v,
		dynamicconfig.WithValidator(validate),
	)
}

func main() {
	path := flag.String("config", "config.yaml", "path to the configuration file")

	flag.Parse()

	cfg, err := Load(*path)
	if err != nil {
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	// The engine is not reachable from here, and saying so is the point:
	// no cfg.Viper().GetString(...) can spread through this codebase as a
	// second configuration API, and no goroutine can read the engine
	// while a reload is writing it.
	fmt.Printf("sealed: %v\n", cfg.Sealed())
	fmt.Printf("engine reachable: %v\n", cfg.Viper() != nil)

	// Everything else is unchanged. Sealing hides a route; it does not
	// take anything away.
	current := cfg.Current()

	fmt.Printf("listening on %s:%d\n", current.Server.Host, current.Server.Port)
	fmt.Printf("beta feature: %v\n", current.Features["beta"])

	if err := cfg.Reload(context.Background()); err != nil {
		log.Fatalf("reload: %v", err)
	}

	fmt.Printf("reloaded to generation %d from %s\n",
		cfg.Generation(), cfg.Status().ConfigFile)
}
