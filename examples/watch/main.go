// Command watch reloads its configuration while it runs.
//
// Start it, then edit config.yaml in another terminal and watch the
// generation change:
//
//	go run . -config config.yaml
//
// Write an invalid port to see the last-known-good rule: the error is
// reported, and the process keeps running on the configuration it already
// had.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
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

	return nil
}

func main() {
	path := flag.String("config", "config.yaml", "path to the configuration file")

	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	defer stop()

	cfg, err := dynamicconfig.New[AppConfig](
		dynamicconfig.WithConfigFile[AppConfig](*path),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithLogger[AppConfig](logger),
	)
	if err != nil {
		logger.Error("initialize configuration", "error", err)
		os.Exit(1)
	}

	defer func() { _ = cfg.Close() }()

	// React to changes. Log the generation, never the values: this struct
	// is the kind of thing that grows a password field later.
	changes := cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
		logger.Info("configuration reloaded",
			"generation", change.Generation,
			"source", string(change.Source),
			"port_changed", change.Previous != nil && change.Previous.Server.Port != change.Current.Server.Port,
		)
	})

	defer changes.Unsubscribe()

	failures := cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
		logger.Error("configuration reload rejected, still running on the previous one",
			"stage", string(e.Stage),
			"error", e.Err,
		)
	})

	defer failures.Unsubscribe()

	// The watcher blocks, so it runs on a goroutine the application owns
	// and can see.
	go func() {
		if err := cfg.Watch(ctx); err != nil &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, dynamicconfig.ErrClosed) {
			logger.Error("configuration watcher stopped", "error", err)
		}
	}()

	// SIGHUP asks for a reload without any support from this package: the
	// manual and automatic paths are the same transaction.
	hangups := make(chan os.Signal, 1)

	signal.Notify(hangups, syscall.SIGHUP)

	go func() {
		for range hangups {
			if err := cfg.Reload(ctx); err != nil {
				logger.Error("manual reload failed", "error", err)
			}
		}
	}()

	run(ctx, cfg)
}

// run is the application. It reads one snapshot per unit of work, which is
// the pattern to copy.
func run(ctx context.Context, cfg *dynamicconfig.Config[AppConfig]) {
	ticker := time.NewTicker(2 * time.Second)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("shutting down")

			return

		case <-ticker.C:
			current := cfg.Current()

			fmt.Printf("generation %d: serving %s:%d, beta=%v\n",
				cfg.Generation(),
				current.Server.Host,
				current.Server.Port,
				current.Features["beta"],
			)
		}
	}
}
