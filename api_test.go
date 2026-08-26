package dynamicconfig_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	dynamicconfig "github.com/ctolon/dynamic-config-go"
	"github.com/spf13/viper"
)

// The public surface, written down as types.
//
// This file compiles rather than runs. Its purpose is to make a change to
// any exported signature a deliberate act: rename a method, reorder a
// parameter or change a return type, and this stops building. That matters
// more than usual here, because the compatibility policy names these
// specific signatures as the thing that will be stable at v1, and a
// promise nothing checks is a promise that drifts.
//
// Adding to the surface is fine and does not break this file. Changing what
// is already here is what it is for.
type publicAPI struct {
	// Constructors, along both axes: who owns the engine, and whether the
	// Config exposes it.
	newOpen    func(...dynamicconfig.Option[appConfig]) (*dynamicconfig.Config[appConfig], error)
	newSealed  func(...dynamicconfig.Option[appConfig]) (*dynamicconfig.Config[appConfig], error)
	wrapOpen   func(*viper.Viper, ...dynamicconfig.Option[appConfig]) (*dynamicconfig.Config[appConfig], error)
	wrapSealed func(*viper.Viper, ...dynamicconfig.Option[appConfig]) (*dynamicconfig.Config[appConfig], error)

	// The runtime surface.
	current     func() *appConfig
	viperOf     func() *viper.Viper
	sealed      func() bool
	reload      func(context.Context) error
	watch       func(context.Context) error
	closeConfig func() error

	subscribe       func(dynamicconfig.ChangeHandler[appConfig]) dynamicconfig.Subscription
	subscribeErrors func(dynamicconfig.ErrorHandler) dynamicconfig.Subscription

	status      func() dynamicconfig.Status
	generation  func() uint64
	reloadCount func() uint64

	// Options.
	withConfigFile   func(string) dynamicconfig.Option[appConfig]
	withOptionalFile func(string) dynamicconfig.Option[appConfig]
	withValidator    func(dynamicconfig.Validator[appConfig]) dynamicconfig.Option[appConfig]
	withDebounce     func(time.Duration) dynamicconfig.Option[appConfig]
	withEventBuffer  func(int) dynamicconfig.Option[appConfig]
	withLogger       func(*slog.Logger) dynamicconfig.Option[appConfig]
	withDecodeOption func(...viper.DecoderConfigOption) dynamicconfig.Option[appConfig]
	withAllowMissing func(bool) dynamicconfig.Option[appConfig]
	withViperSetup   func(func(*viper.Viper) error) dynamicconfig.Option[appConfig]
}

func TestPublicAPISurface(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig(t)

	api := publicAPI{
		newOpen:    dynamicconfig.New[appConfig],
		newSealed:  dynamicconfig.NewSealed[appConfig],
		wrapOpen:   dynamicconfig.Wrap[appConfig],
		wrapSealed: dynamicconfig.WrapSealed[appConfig],

		current:     cfg.Current,
		viperOf:     cfg.Viper,
		sealed:      cfg.Sealed,
		reload:      cfg.Reload,
		watch:       cfg.Watch,
		closeConfig: cfg.Close,

		subscribe:       cfg.Subscribe,
		subscribeErrors: cfg.SubscribeErrors,

		status:      cfg.Status,
		generation:  cfg.Generation,
		reloadCount: cfg.ReloadCount,

		withConfigFile:   dynamicconfig.WithConfigFile[appConfig],
		withOptionalFile: dynamicconfig.WithOptionalConfigFile[appConfig],
		withValidator:    dynamicconfig.WithValidator[appConfig],
		withDebounce:     dynamicconfig.WithDebounce[appConfig],
		withEventBuffer:  dynamicconfig.WithEventBuffer[appConfig],
		withLogger:       dynamicconfig.WithLogger[appConfig],
		withDecodeOption: dynamicconfig.WithDecodeOption[appConfig],
		withAllowMissing: dynamicconfig.WithAllowMissingFile[appConfig],
		withViperSetup:   dynamicconfig.WithViperSetup[appConfig],
	}

	if api.current() == nil {
		t.Fatal("Current is nil")
	}

	// The exported data types, with the fields the policy names.
	var (
		status  dynamicconfig.Status
		change  dynamicconfig.Change[appConfig]
		failure dynamicconfig.ReloadError
	)

	_ = status.Generation
	_ = status.SuccessfulReloads
	_ = status.FailedReloads
	_ = status.LastSuccess
	_ = status.LastFailure
	_ = status.Watching
	_ = status.Closed
	_ = status.ConfigFile
	_ = status.ConfigFiles
	_ = status.DroppedEvents

	_ = change.Previous
	_ = change.Current
	_ = change.Generation
	_ = change.ReloadedAt
	_ = change.Source

	_ = failure.Err
	_ = failure.Stage
	_ = failure.Time
	_ = failure.Generation
	_ = failure.Source

	// The sentinels, and the stage and source vocabularies.
	for _, err := range []error{
		dynamicconfig.ErrClosed,
		dynamicconfig.ErrAlreadyWatching,
		dynamicconfig.ErrNoSnapshot,
		dynamicconfig.ErrNoConfigFile,
		dynamicconfig.ErrInvalidOption,
	} {
		if err == nil {
			t.Fatal("a sentinel error is nil")
		}
	}

	for _, stage := range []dynamicconfig.ReloadStage{
		dynamicconfig.StageRead,
		dynamicconfig.StageDecode,
		dynamicconfig.StageValidation,
		dynamicconfig.StageWatch,
		dynamicconfig.StageCallback,
	} {
		if stage == "" {
			t.Fatal("a reload stage is empty")
		}
	}

	for _, source := range []dynamicconfig.ReloadSource{
		dynamicconfig.ReloadSourceInitial,
		dynamicconfig.ReloadSourceFile,
		dynamicconfig.ReloadSourceManual,
	} {
		if source == "" {
			t.Fatal("a reload source is empty")
		}
	}

	// The documented defaults are part of the contract too.
	if dynamicconfig.DefaultDebounce != 200*time.Millisecond {
		t.Fatalf("DefaultDebounce = %s", dynamicconfig.DefaultDebounce)
	}

	if dynamicconfig.DefaultEventBuffer != 16 {
		t.Fatalf("DefaultEventBuffer = %d", dynamicconfig.DefaultEventBuffer)
	}
}
