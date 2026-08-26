package dynamicconfig_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
	"github.com/spf13/viper"
)

type serverConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type appConfig struct {
	Server   serverConfig    `mapstructure:"server"`
	Features map[string]bool `mapstructure:"features"`
}

func validPort(c *appConfig) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range", c.Server.Port)
	}

	return nil
}

// writeConfig writes contents to path, creating it if necessary.
func writeConfig(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newConfigFile creates a config file in a fresh temporary directory.
func newConfigFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")

	writeConfig(t, path, contents)

	return path
}

const baseConfig = `
server:
  host: localhost
  port: 8080
features:
  beta: false
`

// newTestConfig builds a Config over a file containing baseConfig and
// closes it when the test ends.
func newTestConfig(t *testing.T, opts ...dynamicconfig.Option[appConfig]) (*dynamicconfig.Config[appConfig], string) {
	t.Helper()

	path := newConfigFile(t, baseConfig)

	all := append([]dynamicconfig.Option[appConfig]{
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(validPort),
	}, opts...)

	cfg, err := dynamicconfig.New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = cfg.Close() })

	return cfg, path
}

func TestNewPublishesInitialSnapshot(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	current := cfg.Current()
	if current == nil {
		t.Fatal("Current() is nil after successful New")
	}

	if current.Server.Host != "localhost" || current.Server.Port != 8080 {
		t.Fatalf("unexpected snapshot: %+v", current.Server)
	}

	if got := cfg.Generation(); got != 1 {
		t.Fatalf("generation = %d, want 1 for the initial load", got)
	}

	if got := cfg.ReloadCount(); got != 0 {
		t.Fatalf("reload count = %d, want 0: the initial load is not a reload", got)
	}

	if got := cfg.Status().ConfigFile; got != path {
		t.Fatalf("status config file = %q, want %q", got, path)
	}
}

func TestNewMissingFileFailsFast(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](missing),
	)
	if err == nil {
		t.Fatal("New succeeded with a missing configuration file")
	}

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error does not wrap os.ErrNotExist: %v", err)
	}
}

func TestNewAllowMissingFileUsesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithAllowMissingFile[appConfig](true),
		dynamicconfig.WithViperSetup[appConfig](func(v *viper.Viper) error {
			v.SetDefault("server.port", 9090)

			return nil
		}),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		t.Fatalf("New with an optional missing file: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	if got := cfg.Current().Server.Port; got != 9090 {
		t.Fatalf("port = %d, want the default 9090", got)
	}

	// With no file at all there is nothing to watch, and saying so is
	// better than watching nothing.
	if err := cfg.Watch(t.Context()); !errors.Is(err, dynamicconfig.ErrNoConfigFile) {
		t.Fatalf("Watch = %v, want ErrNoConfigFile", err)
	}
}

func TestNewParseErrorFailsFast(t *testing.T) {
	t.Parallel()

	path := newConfigFile(t, "server: {port: [unterminated\n")

	_, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
	)
	if err == nil {
		t.Fatal("New succeeded on an unparseable file")
	}
}

func TestNewValidationFailureFailsFast(t *testing.T) {
	t.Parallel()

	path := newConfigFile(t, "server:\n  port: -1\n")

	_, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](path),
		dynamicconfig.WithValidator(validPort),
	)
	if err == nil {
		t.Fatal("New succeeded on a configuration that does not validate")
	}

	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("error lost the validator's message: %v", err)
	}
}

func TestInvalidOptionsAreRejected(t *testing.T) {
	t.Parallel()

	path := newConfigFile(t, baseConfig)

	cases := map[string]dynamicconfig.Option[appConfig]{
		"negative debounce": dynamicconfig.WithDebounce[appConfig](-time.Second),
		"zero event buffer": dynamicconfig.WithEventBuffer[appConfig](0),
		"negative buffer":   dynamicconfig.WithEventBuffer[appConfig](-5),
		"empty config file": dynamicconfig.WithConfigFile[appConfig](""),
		"nil validator":     dynamicconfig.WithValidator[appConfig](nil),
		"nil logger":        dynamicconfig.WithLogger[appConfig](nil),
		"nil viper setup":   dynamicconfig.WithViperSetup[appConfig](nil),
		"nil decode option": dynamicconfig.WithDecodeOption[appConfig](nil),
		"nil option itself": nil,
	}

	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := dynamicconfig.New[appConfig](
				dynamicconfig.WithConfigFile[appConfig](path),
				opt,
			)
			if !errors.Is(err, dynamicconfig.ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
		})
	}
}

func TestWrapAdoptsExistingViper(t *testing.T) {
	t.Parallel()

	path := newConfigFile(t, baseConfig)

	v := viper.New()
	v.SetConfigFile(path)
	v.SetDefault("features.gamma", true)

	cfg, err := dynamicconfig.Wrap[appConfig](v, dynamicconfig.WithValidator(validPort))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	if cfg.Viper() != v {
		t.Fatal("Wrap replaced the caller's Viper instance")
	}

	if cfg.Sealed() {
		t.Fatal("Wrap produced a sealed configuration")
	}

	if !cfg.Current().Features["gamma"] {
		t.Fatal("the wrapped instance's defaults did not reach the snapshot")
	}
}

func TestWrapNilViper(t *testing.T) {
	t.Parallel()

	if _, err := dynamicconfig.Wrap[appConfig](nil); !errors.Is(err, dynamicconfig.ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestReloadPublishesNewSnapshot(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	before := cfg.Current()

	writeConfig(t, path, "server:\n  host: example\n  port: 9000\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after := cfg.Current()

	if after == before {
		t.Fatal("Reload published the same pointer")
	}

	if after.Server.Port != 9000 || after.Server.Host != "example" {
		t.Fatalf("unexpected snapshot: %+v", after.Server)
	}

	if before.Server.Port != 8080 {
		t.Fatal("the previous snapshot was mutated in place")
	}

	status := cfg.Status()

	if status.Generation != 2 || status.SuccessfulReloads != 1 || status.FailedReloads != 0 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestFailedReloadKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	for name, contents := range map[string]string{
		"unparseable":     "server: {port: [unterminated\n",
		"does not decode": "server:\n  port: not-a-number\n",
		"invalid":         "server:\n  host: example\n  port: 70000\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, path := newTestConfig(t)

			before := cfg.Current()

			writeConfig(t, path, contents)

			err := cfg.Reload(t.Context())
			if err == nil {
				t.Fatal("Reload accepted a configuration it should have rejected")
			}

			after := cfg.Current()

			if after == nil {
				t.Fatal("a failed reload cleared the published snapshot")
			}

			if after != before {
				t.Fatal("a failed reload replaced the published snapshot")
			}

			status := cfg.Status()

			if status.Generation != 1 {
				t.Fatalf("generation = %d, want 1: a rejected reload publishes nothing", status.Generation)
			}

			if status.FailedReloads != 1 {
				t.Fatalf("failed reloads = %d, want 1", status.FailedReloads)
			}

			if status.LastFailure.IsZero() {
				t.Fatal("last failure time was not recorded")
			}

			// The file is bad, not the configuration: repairing it
			// publishes again.
			writeConfig(t, path, "server:\n  host: repaired\n  port: 8081\n")

			if err := cfg.Reload(t.Context()); err != nil {
				t.Fatalf("Reload after repair: %v", err)
			}

			if cfg.Current().Server.Host != "repaired" {
				t.Fatal("the repaired configuration was not published")
			}
		})
	}
}

func TestReloadReportsStage(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	failures := make(chan dynamicconfig.ReloadError, 4)

	sub := cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) { failures <- e })
	defer sub.Unsubscribe()

	writeConfig(t, path, "server:\n  port: 70000\n")

	if err := cfg.Reload(t.Context()); err == nil {
		t.Fatal("Reload accepted an invalid configuration")
	}

	select {
	case e := <-failures:
		if e.Stage != dynamicconfig.StageValidation {
			t.Fatalf("stage = %q, want %q", e.Stage, dynamicconfig.StageValidation)
		}

		if e.Source != dynamicconfig.ReloadSourceManual {
			t.Fatalf("source = %q, want %q", e.Source, dynamicconfig.ReloadSourceManual)
		}

		if e.Generation != 1 {
			t.Fatalf("generation = %d, want the generation still published", e.Generation)
		}

		if e.Error() == "" || errors.Unwrap(e) == nil {
			t.Fatal("ReloadError does not behave as an error")
		}

	case <-time.After(2 * time.Second):
		t.Fatal("no error event was delivered")
	}
}

func TestSubscribeReceivesChanges(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	changes := make(chan dynamicconfig.Change[appConfig], 4)

	sub := cfg.Subscribe(func(c dynamicconfig.Change[appConfig]) { changes <- c })

	writeConfig(t, path, "server:\n  host: example\n  port: 9000\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	select {
	case change := <-changes:
		if change.Generation != 2 {
			t.Fatalf("generation = %d, want 2", change.Generation)
		}

		if change.Previous == nil || change.Previous.Server.Port != 8080 {
			t.Fatal("the change did not carry the previous snapshot")
		}

		if change.Current.Server.Port != 9000 {
			t.Fatal("the change did not carry the new snapshot")
		}

		if change.Source != dynamicconfig.ReloadSourceManual {
			t.Fatalf("source = %q", change.Source)
		}

		if change.ReloadedAt.IsZero() {
			t.Fatal("the change has no timestamp")
		}

	case <-time.After(2 * time.Second):
		t.Fatal("no change event was delivered")
	}

	// Unsubscribing twice is not an error, and stops delivery.
	sub.Unsubscribe()
	sub.Unsubscribe()

	writeConfig(t, path, "server:\n  host: example\n  port: 9100\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload after unsubscribe: %v", err)
	}

	select {
	case change := <-changes:
		t.Fatalf("event delivered after Unsubscribe: generation %d", change.Generation)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSubscriberPanicIsIsolated(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	callbackErrors := make(chan dynamicconfig.ReloadError, 4)
	survived := make(chan struct{}, 4)

	cfg.Subscribe(func(dynamicconfig.Change[appConfig]) { panic("subscriber exploded") })
	cfg.Subscribe(func(dynamicconfig.Change[appConfig]) { survived <- struct{}{} })

	cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
		if e.Stage == dynamicconfig.StageCallback {
			callbackErrors <- e
		}
	})

	writeConfig(t, path, "server:\n  port: 9000\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	select {
	case <-survived:
	case <-time.After(2 * time.Second):
		t.Fatal("a panicking subscriber stopped delivery to the others")
	}

	select {
	case e := <-callbackErrors:
		if !strings.Contains(e.Err.Error(), "panicked") {
			t.Fatalf("callback error does not mention the panic: %v", e.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a subscriber panic was not reported")
	}

	// The reload machinery is still usable.
	writeConfig(t, path, "server:\n  port: 9200\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload after a subscriber panic: %v", err)
	}

	if cfg.Current().Server.Port != 9200 {
		t.Fatal("the configuration stopped reloading after a subscriber panic")
	}
}

func TestSubscriberMayReload(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t)

	done := make(chan error, 1)

	var once sync.Once

	cfg.Subscribe(func(dynamicconfig.Change[appConfig]) {
		// Callbacks run outside the reload lock, so this must not
		// deadlock. Reload once, or the handler would drive itself.
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			done <- cfg.Reload(ctx)
		})
	})

	writeConfig(t, path, "server:\n  port: 9000\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reload from inside a subscriber: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber calling Reload deadlocked")
	}
}

func TestSlowSubscriberDoesNotBlockPublication(t *testing.T) {
	t.Parallel()

	cfg, path := newTestConfig(t, dynamicconfig.WithEventBuffer[appConfig](2))

	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	cfg.Subscribe(func(dynamicconfig.Change[appConfig]) {
		select {
		case entered <- struct{}{}:
		default:
		}

		<-release
	})

	writeConfig(t, path, "server:\n  port: 9000\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the subscriber never ran")
	}

	// The subscriber is now wedged. Publication must be unaffected.
	for i := range 20 {
		writeConfig(t, path, fmt.Sprintf("server:\n  port: %d\n", 9100+i))

		if err := cfg.Reload(t.Context()); err != nil {
			t.Fatalf("reload %d blocked behind a slow subscriber: %v", i, err)
		}
	}

	if got := cfg.Current().Server.Port; got != 9119 {
		t.Fatalf("port = %d, want 9119: publication did not keep up", got)
	}

	if dropped := cfg.Status().DroppedEvents; dropped == 0 {
		t.Fatal("the bounded queue never reported a drop despite a wedged subscriber")
	}

	close(release)
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	for range 3 {
		if err := cfg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	if !cfg.Status().Closed {
		t.Fatal("status does not report the config as closed")
	}

	if cfg.Current() == nil {
		t.Fatal("Close cleared the published snapshot")
	}

	if err := cfg.Reload(t.Context()); !errors.Is(err, dynamicconfig.ErrClosed) {
		t.Fatalf("Reload after Close = %v, want ErrClosed", err)
	}

	if err := cfg.Watch(t.Context()); !errors.Is(err, dynamicconfig.ErrClosed) {
		t.Fatalf("Watch after Close = %v, want ErrClosed", err)
	}

	// Subscribing to a closed config is accepted and delivers nothing.
	cfg.Subscribe(func(dynamicconfig.Change[appConfig]) {
		t.Error("a closed config delivered an event")
	}).Unsubscribe()
}

func TestReloadHonoursContext(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := cfg.Reload(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reload = %v, want context.Canceled", err)
	}

	if cfg.Current() == nil {
		t.Fatal("a cancelled reload disturbed the snapshot")
	}
}

func TestSingleWatcher(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watching := make(chan error, 1)

	go func() { watching <- cfg.Watch(ctx) }()

	waitFor(t, func() bool { return cfg.Status().Watching }, "watcher to start")

	if err := cfg.Watch(t.Context()); !errors.Is(err, dynamicconfig.ErrAlreadyWatching) {
		t.Fatalf("second Watch = %v, want ErrAlreadyWatching", err)
	}

	cancel()

	select {
	case err := <-watching:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}

	waitFor(t, func() bool { return !cfg.Status().Watching }, "watcher to be reported as stopped")
}

func TestCloseStopsWatch(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	watching := make(chan error, 1)

	// A context that is never cancelled: Close alone must stop the
	// watcher.
	go func() { watching <- cfg.Watch(context.Background()) }()

	waitFor(t, func() bool { return cfg.Status().Watching }, "watcher to start")

	if err := cfg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-watching:
		if !errors.Is(err, dynamicconfig.ErrClosed) {
			t.Fatalf("Watch = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the watcher")
	}
}

func TestViperStateAndSnapshotAreDistinct(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	cfg.Viper().Set("server.port", -1)

	if got := cfg.Viper().GetInt("server.port"); got != -1 {
		t.Fatalf("viper port = %d, want the value just set", got)
	}

	if got := cfg.Current().Server.Port; got != 8080 {
		t.Fatalf("snapshot port = %d: setting a value on Viper must not publish it", got)
	}

	// Publishing takes a reload, and the invalid value is then rejected.
	if err := cfg.Reload(t.Context()); err == nil {
		t.Fatal("an invalid value set on Viper was accepted")
	}

	if got := cfg.Current().Server.Port; got != 8080 {
		t.Fatalf("snapshot port = %d after a rejected reload", got)
	}
}

// waitFor polls until cond holds, failing the test if it never does.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// A layered configuration: one struct, one instance, several files, later
// files overriding earlier ones. It is the shape a deployment usually
// wants — a checked-in base, a secret mounted separately, an optional local
// override — without giving up a single snapshot, a single validation or a
// single generation.

const layeredBase = `
server:
  host: localhost
  port: 8080
features:
  beta: false
`

func TestLayeredFilesMergeInOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	base := filepath.Join(dir, "config.yaml")
	secret := filepath.Join(dir, "secret.yaml")

	writeConfig(t, base, layeredBase)
	writeConfig(t, secret, "server:\n  port: 9443\nfeatures:\n  beta: true\n")

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](base),
		dynamicconfig.WithConfigFile[appConfig](secret),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	current := cfg.Current()

	// The later file wins where they overlap...
	if current.Server.Port != 9443 {
		t.Fatalf("port = %d, want 9443 from the second file", current.Server.Port)
	}

	if !current.Features["beta"] {
		t.Fatal("the second file did not override the first")
	}

	// ...and leaves alone what it does not mention.
	if current.Server.Host != "localhost" {
		t.Fatalf("host = %q, want the base file's value", current.Server.Host)
	}

	status := cfg.Status()

	if got := status.ConfigFiles; len(got) != 2 || got[0] != base || got[1] != secret {
		t.Fatalf("status files = %v, want both in layering order", got)
	}

	if status.ConfigFile != base {
		t.Fatalf("primary file = %q, want the first layer", status.ConfigFile)
	}

	// One generation, however many files it came from.
	if status.Generation != 1 {
		t.Fatalf("generation = %d, want 1", status.Generation)
	}
}

func TestLayeredReloadRereadsEveryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	base := filepath.Join(dir, "config.yaml")
	secret := filepath.Join(dir, "secret.yaml")

	writeConfig(t, base, layeredBase)
	writeConfig(t, secret, "server:\n  port: 9443\n")

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](base),
		dynamicconfig.WithConfigFile[appConfig](secret),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	// A change to the lower layer that the upper one does not override.
	writeConfig(t, base, "server:\n  host: changed\n  port: 8080\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := cfg.Current().Server.Host; got != "changed" {
		t.Fatalf("host = %q, want the base file's new value", got)
	}

	if got := cfg.Current().Server.Port; got != 9443 {
		t.Fatalf("port = %d: the upper layer stopped applying", got)
	}

	// A key removed from the upper layer stops overriding, rather than
	// surviving from the previous read.
	writeConfig(t, secret, "features:\n  beta: true\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := cfg.Current().Server.Port; got != 8080 {
		t.Fatalf("port = %d, want the base value once the override was removed", got)
	}
}

func TestOptionalLayerMayBeAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	base := filepath.Join(dir, "config.yaml")
	local := filepath.Join(dir, "config.local.yaml")

	writeConfig(t, base, layeredBase)

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](base),
		dynamicconfig.WithOptionalConfigFile[appConfig](local),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		t.Fatalf("New with an absent optional layer: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	if got := cfg.Current().Server.Port; got != 8080 {
		t.Fatalf("port = %d, want the base value", got)
	}

	if got := cfg.Status().ConfigFiles; len(got) != 1 {
		t.Fatalf("status files = %v, want only the file that exists", got)
	}

	// The layer appearing later is picked up by the next reload.
	writeConfig(t, local, "server:\n  port: 9999\n")

	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := cfg.Current().Server.Port; got != 9999 {
		t.Fatalf("port = %d, want the optional layer's value", got)
	}

	if got := cfg.Status().ConfigFiles; len(got) != 2 {
		t.Fatalf("status files = %v, want both once the optional layer exists", got)
	}
}

func TestRequiredLayerMissingFailsFast(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	base := filepath.Join(dir, "config.yaml")

	writeConfig(t, base, layeredBase)

	_, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](base),
		dynamicconfig.WithConfigFile[appConfig](filepath.Join(dir, "absent.yaml")),
	)
	if err == nil {
		t.Fatal("New succeeded with a required layer missing")
	}

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error does not wrap os.ErrNotExist: %v", err)
	}
}

func TestDeletedLayerKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	base := filepath.Join(dir, "config.yaml")
	secret := filepath.Join(dir, "secret.yaml")

	writeConfig(t, base, layeredBase)
	writeConfig(t, secret, "server:\n  port: 9443\n")

	cfg, err := dynamicconfig.New[appConfig](
		dynamicconfig.WithConfigFile[appConfig](base),
		dynamicconfig.WithConfigFile[appConfig](secret),
		dynamicconfig.WithValidator(validPort),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() { _ = cfg.Close() }()

	good := cfg.Current()

	// A secret file that disappears must not silently demote the service
	// to whatever the base file says.
	if err := os.Remove(secret); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := cfg.Reload(t.Context()); err == nil {
		t.Fatal("a deleted required layer was accepted")
	}

	if cfg.Current() != good {
		t.Fatal("a deleted layer disturbed the published snapshot")
	}

	if got := cfg.Current().Server.Port; got != 9443 {
		t.Fatalf("port = %d: the service fell back to the base file", got)
	}
}
