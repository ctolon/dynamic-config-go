// Package benchmarks measures the operations whose cost the package makes
// promises about.
//
// The promise that matters is on Current: an application is expected to
// call it on every request, so it must cost an atomic load and allocate
// nothing. The rest are here to keep an eye on the reload path, which is
// allowed to be expensive but not to become surprising.
package benchmarks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
)

type server struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type database struct {
	URL         string `mapstructure:"url"`
	MaxOpen     int    `mapstructure:"max_open"`
	MaxIdle     int    `mapstructure:"max_idle"`
	ConnTimeout string `mapstructure:"conn_timeout"`
}

type config struct {
	Server   server          `mapstructure:"server"`
	Database database        `mapstructure:"database"`
	Features map[string]bool `mapstructure:"features"`
	Tags     []string        `mapstructure:"tags"`
}

const document = `
server:
  host: localhost
  port: 8080
database:
  url: postgres://localhost/app
  max_open: 32
  max_idle: 8
  conn_timeout: 5s
features:
  alpha: true
  beta: false
  gamma: true
tags:
  - one
  - two
  - three
`

func validate(c *config) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range", c.Server.Port)
	}

	if c.Database.MaxIdle > c.Database.MaxOpen {
		return fmt.Errorf("database.max_idle %d exceeds max_open %d", c.Database.MaxIdle, c.Database.MaxOpen)
	}

	return nil
}

func newConfig(tb testing.TB, opts ...dynamicconfig.Option[config]) *dynamicconfig.Config[config] {
	tb.Helper()

	path := filepath.Join(tb.TempDir(), "config.yaml")

	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		tb.Fatalf("write: %v", err)
	}

	all := append([]dynamicconfig.Option[config]{
		dynamicconfig.WithConfigFile[config](path),
	}, opts...)

	cfg, err := dynamicconfig.New(all...)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}

	tb.Cleanup(func() { _ = cfg.Close() })

	return cfg
}

func BenchmarkCurrent(b *testing.B) {
	cfg := newConfig(b)

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = cfg.Current()
	}
}

func BenchmarkCurrentParallel(b *testing.B) {
	cfg := newConfig(b)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = cfg.Current()
		}
	})
}

// BenchmarkCurrentFieldAccess measures the shape an application actually
// uses: one snapshot, several fields.
func BenchmarkCurrentFieldAccess(b *testing.B) {
	cfg := newConfig(b)

	b.ReportAllocs()
	b.ResetTimer()

	var sink int

	for b.Loop() {
		current := cfg.Current()

		sink += current.Server.Port + current.Database.MaxOpen
	}

	runtimeSink = sink
}

func BenchmarkStatus(b *testing.B) {
	cfg := newConfig(b)

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = cfg.Status()
	}
}

func BenchmarkReload(b *testing.B) {
	cfg := newConfig(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := cfg.Reload(ctx); err != nil {
			b.Fatalf("Reload: %v", err)
		}
	}
}

func BenchmarkReloadWithValidation(b *testing.B) {
	cfg := newConfig(b, dynamicconfig.WithValidator(validate))
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := cfg.Reload(ctx); err != nil {
			b.Fatalf("Reload: %v", err)
		}
	}
}

// BenchmarkReloadWithSubscribers measures what a fan-out of subscribers
// adds to the reload path. It should be close to nothing: the reload
// enqueues one notification and returns, and the handlers run elsewhere.
func BenchmarkReloadWithSubscribers(b *testing.B) {
	for _, subscribers := range []int{0, 1, 8, 64} {
		b.Run(fmt.Sprintf("subscribers=%d", subscribers), func(b *testing.B) {
			cfg := newConfig(b, dynamicconfig.WithValidator(validate))

			for range subscribers {
				cfg.Subscribe(func(dynamicconfig.Change[config]) {})
			}

			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				if err := cfg.Reload(ctx); err != nil {
					b.Fatalf("Reload: %v", err)
				}
			}
		})
	}
}

func BenchmarkSubscribe(b *testing.B) {
	cfg := newConfig(b)

	handler := func(dynamicconfig.Change[config]) {}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		cfg.Subscribe(handler).Unsubscribe()
	}
}

// TestCurrentDoesNotAllocate turns the headline promise into a test, so
// that a regression fails a build rather than showing up in a benchmark
// nobody read.
func TestCurrentDoesNotAllocate(t *testing.T) {
	cfg := newConfig(t)

	allocs := testing.AllocsPerRun(1000, func() {
		_ = cfg.Current()
	})

	if allocs != 0 {
		t.Fatalf("Current() allocates %.1f times per call, want 0", allocs)
	}
}

// runtimeSink keeps the compiler from eliding the work in
// BenchmarkCurrentFieldAccess.
var runtimeSink int
