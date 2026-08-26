package dynamicconfig

import (
	"fmt"
	"time"
)

// ReloadSource records what caused a reload. It is metadata for
// observability, not a provenance system: the package tracks who asked, not
// which key came from where.
type ReloadSource string

// The reload sources.
const (
	// ReloadSourceInitial is the load performed by New or Wrap.
	ReloadSourceInitial ReloadSource = "initial"

	// ReloadSourceFile is a reload triggered by a filesystem event.
	ReloadSourceFile ReloadSource = "filesystem"

	// ReloadSourceManual is a reload requested through Reload.
	ReloadSourceManual ReloadSource = "manual"
)

// Change describes one successful publication.
//
// Previous is nil for the initial load. Both pointers address immutable
// snapshots: a handler must not write through them, and must not assume it
// is the only holder — see the snapshot contract in doc.go.
//
// Change deliberately has no String or MarshalJSON method. Rendering it
// would render configuration values, and a configuration struct is exactly
// the kind of thing that holds a database password. Handlers that want to
// log a change should log Generation and Source.
type Change[T any] struct {
	// Previous is the snapshot this change replaced, or nil for the
	// initial load.
	Previous *T

	// Current is the snapshot now published. It is the same pointer
	// Current returns until the next successful reload.
	Current *T

	// Generation is the number of snapshots published so far, counting
	// this one. The initial load is generation 1.
	Generation uint64

	// ReloadedAt is when the snapshot was published.
	ReloadedAt time.Time

	// Source is what triggered the reload.
	Source ReloadSource
}

// ReloadStage names the step of the reload transaction that failed.
type ReloadStage string

// The reload stages, in the order they run.
const (
	// StageRead covers reading the configuration file into Viper.
	StageRead ReloadStage = "read"

	// StageDecode covers decoding Viper's state into T.
	StageDecode ReloadStage = "decode"

	// StageValidation covers the validator's verdict on the decoded value.
	StageValidation ReloadStage = "validation"

	// StageWatch covers a failure of the filesystem watcher itself, rather
	// than of a reload it triggered.
	StageWatch ReloadStage = "watch"

	// StageCallback covers a panic in a subscriber. The reload itself
	// succeeded; only the handler failed.
	StageCallback ReloadStage = "callback"
)

// ReloadError reports a failure to an error subscriber.
//
// A ReloadError at StageRead, StageDecode or StageValidation means the
// candidate configuration was rejected and the previously published
// snapshot is still current. It never means the configuration was lost.
//
// Err carries the cause and never carries configuration values: messages
// name paths, stages and types, so that logging an error cannot route
// around a redaction the application performs elsewhere.
type ReloadError struct {
	// Err is the underlying failure.
	Err error

	// Stage is the step that failed.
	Stage ReloadStage

	// Time is when the failure was recorded.
	Time time.Time

	// Generation is the generation still published at the time of the
	// failure — that is, the configuration the application is still
	// running on.
	Generation uint64

	// Source is what triggered the reload that failed.
	Source ReloadSource
}

// Error implements error.
func (e ReloadError) Error() string {
	return fmt.Sprintf(
		"dynamicconfig: reload failed at stage %q (source %q, generation %d): %v",
		e.Stage,
		e.Source,
		e.Generation,
		e.Err,
	)
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e ReloadError) Unwrap() error {
	return e.Err
}

// ChangeHandler receives successful publications.
//
// Handlers run on a dispatcher goroutine, one at a time, in publication
// order. They must not block indefinitely: delivery is bounded and a
// handler that never returns stops every later event reaching every
// subscriber. A handler that panics is isolated — the panic is reported as
// a StageCallback error and nothing else is disturbed.
type ChangeHandler[T any] func(Change[T])

// ErrorHandler receives reload failures.
//
// The same execution rules apply as for ChangeHandler, with one exception:
// a panic in an error handler is logged but not re-reported as an error, so
// that a broken handler cannot feed itself.
type ErrorHandler func(ReloadError)
