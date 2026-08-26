// Command environment configures itself from defaults and environment
// variables, with the file optional.
//
//	APP_SERVER_PORT=9090 APP_DATABASE_URL=postgres://db/app go run .
//
// This is the shape a twelve-factor deployment usually wants: a file if
// there is one, environment variables on top, and defaults underneath.
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
	"github.com/spf13/viper"
)

// AppConfig is the shape the application expects.
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
	URL string `mapstructure:"url"`
}

func validate(c *AppConfig) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is outside 1-65535", c.Server.Port)
	}

	if c.Database.URL == "" {
		return fmt.Errorf("database.url is empty; set APP_DATABASE_URL")
	}

	return nil
}

func main() {
	cfg, err := dynamicconfig.New[AppConfig](
		// Everything about sources, defaults and the environment is
		// Viper's API, used directly. This package does not mirror it.
		dynamicconfig.WithViperSetup[AppConfig](func(v *viper.Viper) error {
			v.SetConfigName("config")
			v.SetConfigType("yaml")
			v.AddConfigPath(".")
			v.AddConfigPath("/etc/myapp")

			v.SetDefault("server.host", "0.0.0.0")
			v.SetDefault("server.port", 8080)

			v.SetEnvPrefix("APP")
			v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
			v.AutomaticEnv()

			// AutomaticEnv only covers keys Viper already knows about,
			// so a key that exists nowhere else is bound explicitly.
			return v.BindEnv("database.url", "APP_DATABASE_URL")
		}),

		// There may be no file at all, and that is not an error here.
		// Once a file has been read, though, its later disappearance is
		// always a failed reload rather than a silent demotion to
		// defaults.
		dynamicconfig.WithAllowMissingFile[AppConfig](true),

		dynamicconfig.WithValidator(validate),
	)
	if err != nil {
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	current := cfg.Current()

	fmt.Printf("listening on %s:%d\n", current.Server.Host, current.Server.Port)
	fmt.Printf("database configured: %v\n", current.Database.URL != "")

	if file := cfg.Status().ConfigFile; file == "" {
		fmt.Println("no configuration file: running on defaults and the environment")
	} else {
		fmt.Printf("configuration file: %s\n", file)
	}

	// An environment-only configuration still reloads on demand — it just
	// has nothing on disk to watch.
	if err := cfg.Reload(context.Background()); err != nil {
		log.Fatalf("reload: %v", err)
	}
}
