// Command validation shows the validator deciding what may be published.
//
// It loads a configuration, then deliberately publishes a bad value through
// Viper to demonstrate that the snapshot does not follow it.
//
//	go run . -config config.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"strings"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

// AppConfig is validated as a whole, not field by field, which is what lets
// rules span fields.
type AppConfig struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
}

// ServerConfig is where the process listens.
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

// DatabaseConfig is how the process reaches its database.
type DatabaseConfig struct {
	URL     string `mapstructure:"url"`
	MaxOpen int    `mapstructure:"max_open"`
	MaxIdle int    `mapstructure:"max_idle"`
}

// validate is ordinary Go. No validation framework, no struct tags, no
// dependency — and every rule the application actually cares about,
// including the ones that relate two fields to each other.
func validate(c *AppConfig) error {
	var problems []string

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		problems = append(problems, fmt.Sprintf("server.port %d is outside 1-65535", c.Server.Port))
	}

	if c.Server.Host == "" {
		problems = append(problems, "server.host is empty")
	}

	if c.Database.URL == "" {
		problems = append(problems, "database.url is empty")
	} else if _, err := url.Parse(c.Database.URL); err != nil {
		// Report the failure, never the value: a database URL usually
		// carries a password.
		problems = append(problems, "database.url is not a valid URL")
	}

	if c.Database.MaxIdle > c.Database.MaxOpen {
		problems = append(problems, fmt.Sprintf(
			"database.max_idle %d exceeds database.max_open %d",
			c.Database.MaxIdle, c.Database.MaxOpen,
		))
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	return nil
}

func main() {
	path := flag.String("config", "config.yaml", "path to the configuration file")

	flag.Parse()

	cfg, err := dynamicconfig.New[AppConfig](
		dynamicconfig.WithConfigFile[AppConfig](*path),
		dynamicconfig.WithValidator(validate),
	)
	if err != nil {
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	fmt.Printf("loaded: port %d, generation %d\n", cfg.Current().Server.Port, cfg.Generation())

	// Now the part worth watching. Setting a value on Viper changes what
	// Viper reports immediately...
	cfg.Viper.Set("server.port", -1)

	fmt.Printf("viper now reports port %d\n", cfg.Viper.GetInt("server.port"))
	fmt.Printf("the snapshot still reports port %d\n", cfg.Current().Server.Port)

	// ...but publishing it takes a reload, and the validator refuses.
	if err := cfg.Reload(context.Background()); err != nil {
		fmt.Printf("reload rejected: %v\n", err)
	}

	fmt.Printf("after the rejected reload, port is still %d (generation %d)\n",
		cfg.Current().Server.Port, cfg.Generation())

	status := cfg.Status()

	fmt.Printf("status: %d successful, %d failed\n", status.SuccessfulReloads, status.FailedReloads)
}
