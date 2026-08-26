// Command multi-file shows the two ways a service reads more than one
// configuration file.
//
//	go run .
//
// They answer different questions, and picking the wrong one is the usual
// source of trouble:
//
//   - **Layered** — one struct, one instance, several files merged into a
//     single snapshot. Use it when the files are *parts of one
//     configuration*: a checked-in base, a secret mounted separately, an
//     optional local override. One validation, one generation, one
//     `Current()`.
//
//   - **Separate** — one instance per file, each with its own struct. Use it
//     when the files are *different configurations* that happen to live in
//     the same process: they change independently, they are owned by
//     different teams, and one being broken should not hold up the other.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

func main() {
	dir, err := os.MkdirTemp("", "multi-file")
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	writeExampleFiles(dir)

	fmt.Println("── layered: one struct, one instance, three files")
	layered(dir)

	fmt.Println()
	fmt.Println("── separate: one instance per configuration")
	separate(dir)
}

// ---------------------------------------------------------------- layered

// AppConfig is one configuration that happens to arrive in several files.
// Nothing in it says which field came from where, and nothing needs to.
type AppConfig struct {
	Server   ServerConfig    `mapstructure:"server"`
	Database DatabaseConfig  `mapstructure:"database"`
	Features map[string]bool `mapstructure:"features"`
}

// ServerConfig is where the process listens.
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

// DatabaseConfig is how the process reaches its database. The DSN arrives
// from the secret file; everything else from the base file.
type DatabaseConfig struct {
	DSN     string `mapstructure:"dsn"`
	MaxOpen int    `mapstructure:"max_open"`
	MaxIdle int    `mapstructure:"max_idle"`
}

// validateApp validates the merged result, not the individual files, which
// is the point: a rule spanning two files has somewhere to live.
func validateApp(c *AppConfig) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is outside 1-65535", c.Server.Port)
	}

	if c.Database.DSN == "" {
		return errors.New("database.dsn is empty; is the secret file mounted?")
	}

	// Report the failure, never the value: a DSN carries a password.
	if _, err := url.Parse(c.Database.DSN); err != nil {
		return errors.New("database.dsn is not a valid URL")
	}

	if c.Database.MaxIdle > c.Database.MaxOpen {
		return fmt.Errorf("database.max_idle %d exceeds database.max_open %d",
			c.Database.MaxIdle, c.Database.MaxOpen)
	}

	return nil
}

func layered(dir string) {
	cfg, err := dynamicconfig.NewSealed[AppConfig](
		// Layers, in order. Later files override earlier ones, and a key
		// nobody overrides keeps the value the base file gave it.
		dynamicconfig.WithConfigFile[AppConfig](filepath.Join(dir, "config.yaml")),
		dynamicconfig.WithConfigFile[AppConfig](filepath.Join(dir, "secret.yaml")),

		// A developer's local override, which production will not have.
		// Absent is legal; present and broken is not.
		dynamicconfig.WithOptionalConfigFile[AppConfig](filepath.Join(dir, "config.local.yaml")),

		dynamicconfig.WithValidator(validateApp),
	)
	if err != nil {
		log.Fatalf("initialize configuration: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	current := cfg.Current()

	fmt.Printf("   listening on %s:%d\n", current.Server.Host, current.Server.Port)
	fmt.Printf("   database configured: %v (pool %d/%d)\n",
		current.Database.DSN != "", current.Database.MaxIdle, current.Database.MaxOpen)
	fmt.Printf("   beta feature: %v\n", current.Features["beta"])

	// One snapshot, one generation, however many files it came from — and
	// the watcher follows every one of them.
	status := cfg.Status()

	fmt.Printf("   generation %d from %d files:\n", status.Generation, len(status.ConfigFiles))

	for _, file := range status.ConfigFiles {
		fmt.Printf("     %s\n", filepath.Base(file))
	}

	fmt.Println("   (the local override supplied the port; the secret supplied the DSN)")
}

// --------------------------------------------------------------- separate

// TelemetryConfig is a configuration of its own: owned elsewhere, changed on
// its own schedule, and harmless to the rest of the process when it is
// briefly wrong.
type TelemetryConfig struct {
	Endpoint    string  `mapstructure:"endpoint"`
	SampleRatio float64 `mapstructure:"sample_ratio"`
}

func validateTelemetry(c *TelemetryConfig) error {
	if c.Endpoint == "" {
		return errors.New("endpoint is empty")
	}

	if !strings.HasPrefix(c.Endpoint, "http://") && !strings.HasPrefix(c.Endpoint, "https://") {
		return errors.New("endpoint must be an http or https URL")
	}

	if c.SampleRatio < 0 || c.SampleRatio > 1 {
		return fmt.Errorf("sample_ratio %v is outside 0-1", c.SampleRatio)
	}

	return nil
}

func separate(dir string) {
	// Two independent configurations. Each has its own struct, its own
	// validator, its own generation counter and its own watcher — so a
	// broken telemetry file cannot stop the application configuration
	// reloading, and neither one's validation rules have to know the
	// other exists.
	app, err := dynamicconfig.NewSealed[AppConfig](
		dynamicconfig.WithConfigFile[AppConfig](filepath.Join(dir, "config.yaml")),
		dynamicconfig.WithConfigFile[AppConfig](filepath.Join(dir, "secret.yaml")),
		dynamicconfig.WithValidator(validateApp),
	)
	if err != nil {
		log.Fatalf("initialize application configuration: %v", err)
	}

	defer func() { _ = app.Close() }()

	telemetry, err := dynamicconfig.NewSealed[TelemetryConfig](
		dynamicconfig.WithConfigFile[TelemetryConfig](filepath.Join(dir, "telemetry.yaml")),
		dynamicconfig.WithValidator(validateTelemetry),
	)
	if err != nil {
		log.Fatalf("initialize telemetry configuration: %v", err)
	}

	defer func() { _ = telemetry.Close() }()

	fmt.Printf("   application: %s:%d (generation %d)\n",
		app.Current().Server.Host, app.Current().Server.Port, app.Generation())

	fmt.Printf("   telemetry:   %s at %.2f (generation %d)\n",
		telemetry.Current().Endpoint, telemetry.Current().SampleRatio, telemetry.Generation())

	// Breaking one leaves the other alone — which is the whole reason to
	// keep them apart.
	if err := os.WriteFile(filepath.Join(dir, "telemetry.yaml"),
		[]byte("endpoint: not-a-url\nsample_ratio: 9\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	if err := telemetry.Reload(context.Background()); err != nil {
		fmt.Printf("   telemetry reload rejected: %v\n", err)
	}

	fmt.Printf("   telemetry still at %.2f, application still on generation %d\n",
		telemetry.Current().SampleRatio, app.Generation())
}

// ------------------------------------------------------------------ setup

func writeExampleFiles(dir string) {
	files := map[string]string{
		// The base: checked into the repository, no secrets in it.
		"config.yaml": `
server:
  host: 127.0.0.1
  port: 8080
database:
  max_open: 32
  max_idle: 8
features:
  beta: false
`,
		// The secret: mounted separately, never checked in. It carries
		// the one field the base file deliberately leaves empty.
		"secret.yaml": `
database:
  dsn: postgres://app:hunter2@db.internal:5432/app
`,
		// A developer's override: absent in production.
		"config.local.yaml": `
server:
  port: 8888
features:
  beta: true
`,
		// A configuration of its own, on its own schedule.
		"telemetry.yaml": `
endpoint: https://otel.internal:4318
sample_ratio: 0.05
`,
	}

	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			log.Fatal(err)
		}
	}
}
