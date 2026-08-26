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
	_, err := c.runTransaction(ReloadSourceInitial)
	if err == nil {
		return nil
	}

	var failure *ReloadError

	if errors.As(err, &failure) {
		c.failedReloads.Add(1)
		storeTime(&c.lastFailure, failure.Time)

		return fmt.Errorf("dynamicconfig: initial load: %w", failure.Err)
	}

	return fmt.Errorf("dynamicconfig: initial load: %w", err)
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

	change, err := c.runTransaction(source)

	// Release before anything that touches user code or user-visible
	// queues. Invariant: no subscriber callback ever runs while the
	// reload semaphore is held, so a handler may call Reload without
	// deadlocking.
	<-c.reloadSem

	if err != nil {
		var failure *ReloadError

		// A rejected candidate is a reload failure: counted, reported,
		// and survivable. Anything else — today, only a Close that won
		// the race to the commit — is not a verdict on the
		// configuration and is neither counted nor reported.
		if errors.As(err, &failure) {
			c.reportFailure(*failure)

			return fmt.Errorf("dynamicconfig: reload: %w", failure.Err)
		}

		return err
	}

	c.dispatchChange(change)

	return nil
}

// runTransaction performs one reload attempt. The caller holds the reload
// semaphore, or is the initial load, which has no competition.
//
// It returns either the published change or an error. A rejected candidate
// is reported as a *ReloadError naming the stage that refused it; a
// shutdown that won the race to the commit is reported as ErrClosed, which
// is not a verdict on the configuration and is not counted as a failure.
func (c *Config[T]) runTransaction(source ReloadSource) (Change[T], error) {
	fail := func(stage ReloadStage, err error) error {
		return &ReloadError{
			Err:        err,
			Stage:      stage,
			Time:       time.Now(),
			Generation: c.generation.Load(),
			Source:     source,
		}
	}

	read, err := c.readIntoViper()
	if err != nil {
		return Change[T]{}, fail(StageRead, err)
	}

	next := new(T)

	if err := c.viper.Unmarshal(next, c.opts.decodeOptions...); err != nil {
		return Change[T]{}, fail(StageDecode, fmt.Errorf("decode configuration into %T: %w", *next, err))
	}

	if err := c.validate(next); err != nil {
		return Change[T]{}, fail(StageValidation, err)
	}

	// Everything that can fail has now succeeded, and none of it happened
	// under a lock.
	change, err := c.commit(next, read, source)
	if err != nil {
		return Change[T]{}, err
	}

	if c.opts.logger != nil {
		// Stages, counters and paths — never values.
		c.opts.logger.Info(
			"dynamicconfig: configuration published",
			"generation", change.Generation,
			"source", string(source),
			"config_file", c.configFileUsed(),
		)
	}

	return change, nil
}

// commit is the publication point, and the only place a snapshot becomes
// visible.
//
// It runs under the publication gate, which it shares with the transition
// to closing — so a snapshot and a shutdown cannot interleave: one of them
// is first, and after Close wins, nothing else is ever published. The
// section holds no file, no decoder and no user code, only the few stores
// that make a generation visible.
func (c *Config[T]) commit(next *T, read readResult, source ReloadSource) (Change[T], error) {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()

	if c.closed() {
		return Change[T]{}, ErrClosed
	}

	previous := c.current.Load()
	now := time.Now()

	generation := c.generation.Add(1)

	c.current.Store(next)

	storeTime(&c.lastSuccess, now)

	if source != ReloadSourceInitial {
		c.successfulReloads.Add(1)
	}

	// The stamps were taken from the files this candidate was read from,
	// so they belong to the snapshot rather than to whatever those files
	// happen to be now — and committing them here means a rejected
	// candidate never moves them.
	if read.files != nil {
		c.configFiles.Store(&read.files)
		c.fileStamps.Store(&read.stamps)
	}

	return Change[T]{
		Previous:   previous,
		Current:    next,
		Generation: generation,
		ReloadedAt: now,
		Source:     source,
	}, nil
}

// validate runs the validator, if there is one, with panic isolation.
//
// A validator is application code on a recoverable path. A panic in one is
// a rejected candidate — the same outcome as a returned error, with the
// same consequence, which is that the running configuration stays exactly
// where it was — rather than a process that dies because somebody
// dereferenced a nil pointer in a rule about port ranges.
func (c *Config[T]) validate(next *T) (err error) {
	if c.opts.validator == nil {
		return nil
	}

	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		c.logPanic("validator", recovered, debug.Stack())

		err = fmt.Errorf("validate configuration: validator panicked: %v", recovered)
	}()

	if verr := c.opts.validator(next); verr != nil {
		return fmt.Errorf("validate configuration: %w", verr)
	}

	return nil
}

// readResult is what the read stage produced: the files that were actually
// read, in layering order, and a stamp for each.
type readResult struct {
	files  []string
	stamps map[string]fileStamp
}

// readIntoViper performs the read stage.
//
// With no files named, Viper's own discovery decides. With files named,
// they are layered in the order the options gave them: the first one that
// is present is read, replacing whatever Viper held — which is what stops a
// key deleted from a file surviving into the next snapshot — and the rest
// are merged over it, so a later file wins a conflict.
//
// A file marked optional may be absent. Anything else missing, unreadable
// or unparseable fails the stage, and a failed stage leaves the published
// snapshot exactly where it was: a deleted secrets file must not silently
// demote a running service to its defaults.
func (c *Config[T]) readIntoViper() (readResult, error) {
	if len(c.opts.configFiles) == 0 {
		return c.readDiscovered()
	}

	result := readResult{stamps: make(map[string]fileStamp, len(c.opts.configFiles))}

	for _, file := range c.opts.configFiles {
		stamp, err := statStamp(file.path)
		if err != nil {
			if file.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return readResult{}, fmt.Errorf("read configuration %s: %w", file.path, err)
		}

		c.viper.SetConfigFile(file.path)

		// The first file read replaces Viper's state; the rest merge
		// into it.
		if len(result.files) == 0 {
			err = c.viper.ReadInConfig()
		} else {
			err = c.viper.MergeInConfig()
		}

		if err != nil {
			return readResult{}, fmt.Errorf("read configuration %s: %w", file.path, err)
		}

		result.files = append(result.files, file.path)
		result.stamps[file.path] = *stamp
	}

	if len(result.files) == 0 {
		// Every named file was optional and every one of them was
		// absent. Tolerating that silently would mean publishing
		// whatever Viper still held from the previous reload.
		if c.opts.allowMissingFile && len(c.configFilesUsed()) == 0 {
			return readResult{}, nil
		}

		return readResult{}, errors.New("read configuration: none of the configured files exist")
	}

	return result, nil
}

// readDiscovered is the path for a Viper instance that finds its own file
// through a config name and search paths.
func (c *Config[T]) readDiscovered() (readResult, error) {
	if err := c.viper.ReadInConfig(); err != nil {
		if c.tolerateMissingFile(err) {
			return readResult{}, nil
		}

		return readResult{}, fmt.Errorf("read configuration: %w", err)
	}

	path := c.viper.ConfigFileUsed()
	if path == "" {
		return readResult{}, nil
	}

	result := readResult{
		files:  []string{path},
		stamps: make(map[string]fileStamp, 1),
	}

	// Taken next to the read, so that the stamp describes the file this
	// candidate came from. A stat and a read are not atomic against an
	// external writer, so this is best-effort by nature — it exists to
	// close the load/watch gap, not to detect every possible change.
	stamp, err := statStamp(path)
	if err != nil {
		return result, nil
	}

	result.stamps[path] = *stamp

	return result, nil
}

// tolerateMissingFile decides whether Viper finding nothing at all is
// allowed. It covers only the discovery path: a named file that may be
// absent is said so with WithOptionalConfigFile.
func (c *Config[T]) tolerateMissingFile(err error) bool {
	if !c.opts.allowMissingFile {
		return false
	}

	if len(c.configFilesUsed()) != 0 {
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

// filesChangedSinceLoad reports whether any file on disk differs from the
// one the published snapshot was read from. A stat error counts as changed:
// if a file cannot be examined, the safe assumption is that a reload is
// owed.
func (c *Config[T]) filesChangedSinceLoad() bool {
	files := c.configFilesUsed()
	if len(files) == 0 {
		return false
	}

	recorded := c.fileStamps.Load()
	if recorded == nil {
		return true
	}

	for _, path := range files {
		previous, ok := (*recorded)[path]
		if !ok {
			return true
		}

		current, err := statStamp(path)
		if err != nil {
			return true
		}

		if *current != previous {
			return true
		}
	}

	return false
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
