// Command http-server serves requests from a configuration that can change
// while it runs.
//
//	go run . -config config.yaml
//	curl localhost:8080/
//	curl localhost:8080/healthz
//
// Edit config.yaml while it runs: the next request sees the new values, and
// no request ever sees half of them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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
	Greeting string          `mapstructure:"greeting"`
	Timeout  time.Duration   `mapstructure:"timeout"`
	Features map[string]bool `mapstructure:"features"`
}

func validate(c *AppConfig) error {
	if c.Greeting == "" {
		return errors.New("greeting is empty")
	}

	if c.Timeout <= 0 || c.Timeout > time.Minute {
		return fmt.Errorf("timeout %s is outside (0, 1m]", c.Timeout)
	}

	return nil
}

func main() {
	path := flag.String("config", "config.yaml", "path to the configuration file")
	addr := flag.String("addr", ":8080", "address to listen on")

	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

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

	go func() {
		if err := cfg.Watch(ctx); err != nil &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, dynamicconfig.ErrClosed) {
			logger.Error("configuration watcher stopped", "error", err)
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("/", greet(cfg))
	mux.HandleFunc("/healthz", health(cfg))

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "addr", *addr)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// greet reads exactly one snapshot per request.
//
// That is the pattern: a request is a unit of work, and a unit of work runs
// on one configuration. Calling Current for each field instead would let a
// reload land in the middle of a request and mix two generations.
func greet(cfg *dynamicconfig.Config[AppConfig]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		current := cfg.Current()

		// The timeout comes from the same snapshot as everything else,
		// so a request cannot get one generation's timeout and another
		// generation's greeting.
		ctx, cancel := context.WithTimeout(r.Context(), current.Timeout)

		defer cancel()

		if err := doWork(ctx); err != nil {
			http.Error(w, "request timed out", http.StatusGatewayTimeout)

			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		fmt.Fprintf(w, "%s\n", current.Greeting)
		fmt.Fprintf(w, "beta feature: %v\n", current.Features["beta"])
		fmt.Fprintf(w, "configuration generation: %d\n", cfg.Generation())
	}
}

// doWork stands in for whatever the handler really does.
func doWork(ctx context.Context) error {
	select {
	case <-time.After(time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// health exposes the configuration's health without exposing the
// configuration. Status carries counters, timestamps and state — never
// values — which is what makes it safe to put on an endpoint.
func health(cfg *dynamicconfig.Config[AppConfig]) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		status := cfg.Status()

		w.Header().Set("Content-Type", "application/json")

		// A configuration whose reloads are failing is worth a warning,
		// but the process is still serving the last good one, so it is
		// not unhealthy.
		if status.FailedReloads > 0 && status.LastFailure.After(status.LastSuccess) {
			w.Header().Set("X-Config-Warning", "last reload was rejected")
		}

		_ = json.NewEncoder(w).Encode(status)
	}
}
