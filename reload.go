package dynamicconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime/debug"
	"time"

	"github.com/spf13/viper"
)

// Reload re-reads, decodes, validates and publishes the configuration.
//
// It is the same transaction the filesystem watcher runs, which is what
// makes it useful beyond tests: an admin endpoint, a SIGHUP handler or an
// application that declines to watch the filesystem at all gets exactly the
// semantics the watcher gets.
//
//	if err := cfg.Reload(ctx); err != nil {
//	    slog.Error("reload failed", "error", err)
//	}
//
// The transaction publishes only at the end, after every step that can
// fail:
//
//	read ──► decode ──► validate ──► publish
//
// so a failure at any stage leaves the published snapshot exactly as it
// was. There is no rollback because there is nothing to roll back. An error
// returned here therefore means "this candidate was rejected", never "the
// configuration is gone" — Current keeps returning the last good snapshot.
//
// Reloads are serialised: a second Reload waits for the first. The wait
// honours ctx, so a caller that cannot afford to block does not have to.
// Reload returns ErrClosed after Close.
func (c *Config[T]) Reload(ctx context.Context) error {
	return c.doReload(ctx, ReloadSourceManual)
}

// initialLoad publishes the first snapshot. It runs before the Config is
// visible to anyone, so it needs neither the semaphore's protection against
// concurrent reloads nor a dispatch — there is no other goroutine and there
// are no subscribers yet.
func (c *Config[T]) initialLoad() error {
	_, failure := c.runTransaction(ReloadSourceInitial)
	if failure != nil {
		c.failedReloads.Add(1)
		storeTime(&c.lastFailure, failure.Time)

		return fmt.Errorf("dynamicconfig: initial load: %w", failure.Err)
	}

	return nil
}

func (c *Config[T]) doReload(ctx context.Context, source ReloadSource) error {
	if c.closed() {
		return ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dynamicconfig: reload: %w", err)
	}

	// Serialise. The one-slot channel is what makes the wait cancellable:
	// a caller with a deadline, and a watcher whose context is being torn
	// down, both need a way out that a mutex would not give them.
	select {
	case c.reloadSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("dynamicconfig: reload: %w", ctx.Err())
	case <-c.life.Done():
		return ErrClosed
	}

	if c.closed() {
		<-c.reloadSem

		return ErrClosed
	}

	change, failure := c.runTransaction(source)

	// Release before anything that touches user code or user-visible
	// queues. Invariant: no subscriber callback ever runs while the
	// reload semaphore is held, so a handler may call Reload without
	// deadlocking.
	<-c.reloadSem

	if failure != nil {
		c.reportFailure(*failure)

		return fmt.Errorf("dynamicconfig: reload: %w", failure.Err)
	}

	c.dispatchChange(change)

	return nil
}

// runTransaction performs one reload attempt. It returns either the
// published change or the failure that stopped it, never both. The caller
// holds the reload semaphore (or is the initial load, which has no
// competition).
func (c *Config[T]) runTransaction(source ReloadSource) (Change[T], *ReloadError) {
	fail := func(stage ReloadStage, err error) *ReloadError {
		return &ReloadError{
			Err:        err,
			Stage:      stage,
			Time:       time.Now(),
			Generation: c.generation.Load(),
			Source:     source,
		}
	}

	if err := c.readIntoViper(); err != nil {
		return Change[T]{}, fail(StageRead, err)
	}

	next := new(T)

	if err := c.Viper.Unmarshal(next, c.opts.decodeOptions...); err != nil {
		return Change[T]{}, fail(StageDecode, fmt.Errorf("decode configuration into %T: %w", *next, err))
	}

	if c.opts.validator != nil {
		if err := c.opts.validator(next); err != nil {
			return Change[T]{}, fail(StageValidation, fmt.Errorf("validate configuration: %w", err))
		}
	}

	// Everything that can fail has now succeeded. Publication is the last
	// step and cannot fail, which is why readers only ever observe
	// snapshots that passed all of it.
	previous := c.current.Load()

	generation := c.generation.Add(1)
	now := time.Now()

	c.current.Store(next)

	storeTime(&c.lastSuccess, now)

	if source != ReloadSourceInitial {
		c.successfulReloads.Add(1)
	}

	c.recordFileStamp()

	if c.opts.logger != nil {
		// Stages, counters and paths — never values.
		c.opts.logger.Info(
			"dynamicconfig: configuration published",
			"generation", generation,
			"source", string(source),
			"config_file", c.configFileUsed(),
		)
	}

	return Change[T]{
		Previous:   previous,
		Current:    next,
		Generation: generation,
		ReloadedAt: now,
		Source:     source,
	}, nil
}

// readIntoViper performs the read stage.
//
// A missing file is tolerated only when the configuration was told its file
// is optional and has never had one. Once a file has been read
// successfully, its disappearance is a failure — a deleted ConfigMap must
// not silently demote a running service to its defaults.
func (c *Config[T]) readIntoViper() error {
	if err := c.Viper.ReadInConfig(); err != nil {
		if c.tolerateMissingFile(err) {
			return nil
		}

		return fmt.Errorf("read configuration: %w", err)
	}

	c.setConfigFileUsed(c.Viper.ConfigFileUsed())

	return nil
}

func (c *Config[T]) tolerateMissingFile(err error) bool {
	if !c.opts.allowMissingFile {
		return false
	}

	if c.configFileUsed() != "" {
		return false
	}

	var notFound viper.ConfigFileNotFoundError

	return errors.As(err, &notFound) || errors.Is(err, fs.ErrNotExist)
}

// fileStamp identifies a file version cheaply, without reading it and
// without holding anything that could be a secret.
//
// Mode is part of it because a chmod is a change the next read cares about
// and the modification time does not record: making a file unreadable
// leaves its mtime alone.
type fileStamp struct {
	size    int64
	modTime int64
	mode    uint32
}

func (c *Config[T]) recordFileStamp() {
	path := c.configFileUsed()
	if path == "" {
		return
	}

	stamp, err := statStamp(path)
	if err != nil {
		return
	}

	c.fileStamp.Store(stamp)
}

func statStamp(path string) (*fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	return &fileStamp{
		size:    info.Size(),
		modTime: info.ModTime().UnixNano(),
		mode:    uint32(info.Mode()),
	}, nil
}

// fileChangedSinceLoad reports whether the file on disk differs from the
// one the published snapshot came from. A stat error counts as changed: if
// the file cannot be examined, the safe assumption is that a reload is
// owed.
func (c *Config[T]) fileChangedSinceLoad() bool {
	path := c.configFileUsed()
	if path == "" {
		return false
	}

	recorded := c.fileStamp.Load()
	if recorded == nil {
		return true
	}

	current, err := statStamp(path)
	if err != nil {
		return true
	}

	return *current != *recorded
}

// reportFailure records a rejected reload and tells the error subscribers.
// The published snapshot is untouched by definition: this runs only on
// paths that never reached publication.
func (c *Config[T]) reportFailure(failure ReloadError) {
	c.failedReloads.Add(1)
	storeTime(&c.lastFailure, failure.Time)

	if c.opts.logger != nil {
		c.opts.logger.Error(
			"dynamicconfig: reload rejected, previous configuration retained",
			"stage", string(failure.Stage),
			"source", string(failure.Source),
			"generation", failure.Generation,
			"error", failure.Err,
		)
	}

	c.emitError(failure)
}

// dispatchChange queues one notification carrying every subscriber, so that
// handlers run in registration order within an event and in publication
// order across events.
func (c *Config[T]) dispatchChange(change Change[T]) {
	handlers := c.changeSubs.snapshot()
	if len(handlers) == 0 {
		return
	}

	c.dispatcher.Submit(func() {
		for _, handler := range handlers {
			c.invokeChangeHandler(handler, change)
		}
	})
}

func (c *Config[T]) emitError(failure ReloadError) {
	handlers := c.errorSubs.snapshot()
	if len(handlers) == 0 {
		return
	}

	c.dispatcher.Submit(func() {
		for _, handler := range handlers {
			c.invokeErrorHandler(handler, failure)
		}
	})
}

// invokeChangeHandler runs one change handler with panic isolation. A
// subscriber that panics loses its own notification and nothing else; the
// panic is reported to the error subscribers as a callback-stage failure,
// which is how a broken handler becomes visible instead of invisible.
func (c *Config[T]) invokeChangeHandler(handler ChangeHandler[T], change Change[T]) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		stack := debug.Stack()

		c.logPanic("change", recovered, stack)

		c.emitError(ReloadError{
			Err:        fmt.Errorf("configuration change handler panicked: %v", recovered),
			Stage:      StageCallback,
			Time:       time.Now(),
			Generation: c.generation.Load(),
			Source:     change.Source,
		})
	}()

	handler(change)
}

// invokeErrorHandler runs one error handler with panic isolation. A panic
// here is logged but not reported as another error event: an error handler
// that panics on every error would otherwise feed itself for ever.
func (c *Config[T]) invokeErrorHandler(handler ErrorHandler, failure ReloadError) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		c.logPanic("error", recovered, debug.Stack())
	}()

	handler(failure)
}

// onCallbackPanic is the dispatcher's backstop. The handler wrappers above
// already recover, so reaching this means the package's own dispatch code
// panicked; it is logged and swallowed rather than allowed to take the
// dispatcher down with it.
func (c *Config[T]) onCallbackPanic(recovered any, stack []byte) {
	c.logPanic("dispatch", recovered, stack)
}

func (c *Config[T]) logPanic(kind string, recovered any, stack []byte) {
	if c.opts.logger == nil {
		return
	}

	c.opts.logger.Error(
		"dynamicconfig: subscriber panic recovered",
		"handler", kind,
		"panic", fmt.Sprint(recovered),
		"stack", string(stack),
	)
}
