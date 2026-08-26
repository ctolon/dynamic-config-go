// Command basic loads a configuration file once and reads it.
//
// Run it from this directory:
//
//	go run . -config config.yaml
package main

import (
	"flag"
	"fmt"
	"log"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

// AppConfig is the shape the application expects. The mapstructure tags are
// Viper's; nothing about them is specific to this package.
type AppConfig struct {
	Server ServerConfig `mapstructure:"server"`
}

// ServerConfig is where the process listens.
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

func main() {
	path := flag.String("config", "config.yaml", "path to the configuration file")

	flag.Parse()

	cfg, err := dynamicconfig.New[AppConfig](
		dynamicconfig.WithConfigFile[AppConfig](*path),
	)
	if err != nil {
		// A service that cannot understand its configuration should not
		// start. This is the whole of the fail-fast policy.
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	// One snapshot, used throughout. It cannot change underneath this
	// function even if a reload happens mid-print.
	current := cfg.Current()

	fmt.Printf("listening on %s:%d\n", current.Server.Host, current.Server.Port)

	// Viper is still right there for anything it already does well.
	fmt.Printf("viper read %s\n", cfg.Viper().ConfigFileUsed())
}
