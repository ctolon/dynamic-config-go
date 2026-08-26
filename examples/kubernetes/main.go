// Command kubernetes is a service configured by a mounted ConfigMap.
//
// Deploy it with the manifests in ./manifests, then edit the ConfigMap:
//
//	kubectl apply -f manifests/
//	kubectl edit configmap example-config
//	kubectl logs -f deploy/example
//
// The pod is not restarted and its UID does not change. The kubelet
// republishes the projected volume, the watcher notices the ..data symlink
// swap, and the process publishes a new snapshot.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

// AppConfig is the shape the application expects.
type AppConfig struct {
	Message  string          `mapstructure:"message"`
	LogLevel string          `mapstructure:"log_level"`
	Features map[string]bool `mapstructure:"features"`
}

func validate(c *AppConfig) error {
	if c.Message == "" {
		return errors.New("message is empty")
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q is not one of debug, info, warn, error", c.LogLevel)
	}

	return nil
}

func main() {
	// The path a ConfigMap volume mount produces. The file itself is a
	// symlink into ..data, which is why the watcher watches the
	// directory.
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "/etc/example/config.yaml"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	defer stop()

	cfg, err := dynamicconfig.New[AppConfig](
		dynamicconfig.WithConfigFile[AppConfig](path),
		dynamicconfig.WithValidator(validate),
		dynamicconfig.WithLogger[AppConfig](logger),
	)
	if err != nil {
		// CrashLoopBackOff on bad configuration is the correct
		// behaviour: a pod that cannot understand its ConfigMap should
		// not report itself ready.
		logger.Error("initialize configuration", "error", err)
		os.Exit(1)
	}

	defer func() { _ = cfg.Close() }()

	cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
		logger.Info("configuration reloaded from the mounted volume",
			"generation", change.Generation,
			"source", string(change.Source),
		)
	})

	cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
		logger.Error("configuration reload rejected; still serving the previous ConfigMap",
			"stage", string(e.Stage),
			"error", e.Err,
		)
	})

	go func() {
		if err := cfg.Watch(ctx); err != nil &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, dynamicconfig.ErrClosed) {
			logger.Error("configuration watcher stopped", "error", err)
		}
	}()

	go serveProbes(ctx, cfg, logger)

	ticker := time.NewTicker(10 * time.Second)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")

			return

		case <-ticker.C:
			current := cfg.Current()

			logger.Info("working",
				"message", current.Message,
				"generation", cfg.Generation(),
			)
		}
	}
}

// serveProbes exposes liveness, readiness and the configuration status.
//
// Readiness deliberately stays true when a reload is rejected: the process
// is still serving the last good configuration, and taking it out of the
// load balancer because someone typed a bad value into a ConfigMap would
// turn a harmless mistake into an outage.
func serveProbes(ctx context.Context, cfg *dynamicconfig.Config[AppConfig], logger *slog.Logger) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.Current() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/config/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Counters and timestamps only; no configuration values leave
		// the process here.
		_ = json.NewEncoder(w).Encode(cfg.Status())
	})

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("probe server stopped", "error", err)
	}
}
