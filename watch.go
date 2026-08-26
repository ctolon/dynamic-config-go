package dynamicconfig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/ctolon/dynamic-config-go/internal/debounce"
	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
	"github.com/fsnotify/fsnotify"
)

// kubernetesDataDir is the symlink a projected volume swaps atomically when
// a ConfigMap or Secret is updated. The configuration file in such a mount
// is a symlink into it, so the interesting event names the link, not the
// file.
const kubernetesDataDir = "..data"

// rewatchInterval is how often the watcher tries to re-establish a watch on
// a directory that disappeared. Deliberately unhurried: the case is rare
// and there is nothing to gain from spinning.
const rewatchInterval = time.Second

// Watch watches the configuration file and reloads when it changes. It
// blocks.
//
//	go func() {
//	    if err := cfg.Watch(ctx); err != nil && !errors.Is(err, context.Canceled) {
//	        slog.Error("configuration watcher stopped", "error", err)
//	    }
//	}()
//
// Blocking, rather than starting a goroutine internally, is the point: the
// goroutine's lifetime belongs to the application, and is visible at the
// call site.
//
// It returns when ctx is cancelled (with ctx's error), when the Config is
// closed (with ErrClosed), or when the underlying watcher fails
// unrecoverably. A rejected reload is not a reason to stop: bad
// configuration is reported to the error subscribers and the watcher keeps
// waiting for the next write.
//
// Only one watcher may run per Config; a second concurrent call returns
// ErrAlreadyWatching. A Config with no configuration file — one built from
// defaults and the environment — has nothing to watch and returns
// ErrNoConfigFile.
//
// What it handles:
//
//   - bursts of events from one logical write, folded into one reload by
//     the debounce window;
//   - atomic replacement (write to a temporary file, then rename), which is
//     how editors and deployment tools update files;
//   - Kubernetes projected volumes, where the update is a symlink swap
//     rather than a write to the file;
//   - deletion, which reports an error and keeps the last good snapshot,
//     followed by re-creation, which publishes again;
//   - the gap between loading and watching: a change made in that window is
//     detected on startup rather than missed until the next write.
func (c *Config[T]) Watch(ctx context.Context) error {
	switch c.life.BeginWatch() {
	case lifecycle.OutcomeBusy:
		return ErrAlreadyWatching

	case lifecycle.OutcomeClosed:
		return ErrClosed
	}

	defer c.life.EndWatch()

	if c.current.Load() == nil {
		return ErrNoSnapshot
	}

	path := c.configFileUsed()
	if path == "" {
		return ErrNoConfigFile
	}

	return c.watch(ctx, filepath.Clean(path))
}

func (c *Config[T]) watch(ctx context.Context, path string) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("dynamicconfig: watch: create watcher: %w", err)
	}

	defer func() { _ = watcher.Close() }()

	dir := filepath.Dir(path)

	// The directory is watched rather than the file. A file watch follows
	// an inode, and every realistic update — rename-into-place, a
	// projected-volume swap, delete-and-recreate — replaces the inode.
	// Watching the directory survives all three.
	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("dynamicconfig: watch: watch directory %s: %w", dir, err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stopLink := c.cancelOnClose(watchCtx, cancel)
	defer stopLink()

	debouncer := debounce.New(c.opts.debounce)

	var workers sync.WaitGroup

	workers.Add(1)

	go func() {
		defer workers.Done()

		debouncer.Run(watchCtx, func() {
			// A rejected reload is already reported to the error
			// subscribers; the watcher's job is only to keep going.
			_ = c.doReload(watchCtx, ReloadSourceFile)
		})
	}()

	// Close the load/watch gap. The watch is established before this
	// check, so any change from here on produces an event; the check
	// covers the window between the initial load and the line above. The
	// stamp comparison keeps a watcher from republishing a configuration
	// that never changed.
	if c.fileChangedSinceLoad() {
		debouncer.Trigger()
	}

	err = c.watchLoop(watchCtx, watcher, path, dir, debouncer)

	cancel()
	workers.Wait()

	return err
}

// cancelOnClose links Close to the watcher's context, so that Close stops a
// watcher that was started with a context nobody is going to cancel. The
// returned function releases the linking goroutine.
func (c *Config[T]) cancelOnClose(ctx context.Context, cancel context.CancelFunc) func() {
	released := make(chan struct{})

	go func() {
		select {
		case <-c.life.Done():
			cancel()
		case <-ctx.Done():
		case <-released:
		}
	}()

	return sync.OnceFunc(func() { close(released) })
}

func (c *Config[T]) watchLoop(
	ctx context.Context,
	watcher *fsnotify.Watcher,
	path string,
	dir string,
	debouncer *debounce.Debouncer,
) error {
	var (
		retryTimer *time.Timer
		retryC     <-chan time.Time
	)

	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()

	// scheduleRewatch arms the retry that re-establishes a watch on a
	// directory that was removed — the case where a whole config mount is
	// replaced rather than a file within it.
	scheduleRewatch := func() {
		if retryC != nil {
			return
		}

		if retryTimer == nil {
			retryTimer = time.NewTimer(rewatchInterval)
		} else {
			retryTimer.Reset(rewatchInterval)
		}

		retryC = retryTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			if c.closed() {
				return ErrClosed
			}

			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("dynamicconfig: watch: event stream closed")
			}

			if relevant(event, path, dir) {
				debouncer.Trigger()
			}

			if removedDirectory(event, dir) {
				scheduleRewatch()
			}

		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return errors.New("dynamicconfig: watch: error stream closed")
			}

			// Watcher errors are reported, not fatal: an overflowed
			// event queue costs events, and the reload that follows
			// re-reads the file anyway.
			c.emitWatchError(watchErr)

			debouncer.Trigger()

		case <-retryC:
			retryC = nil

			if err := watcher.Add(dir); err != nil {
				scheduleRewatch()

				continue
			}

			// The directory came back. Whatever happened while it was
			// gone was missed, so reload unconditionally.
			debouncer.Trigger()
		}
	}
}

// relevant decides whether an event concerns the configuration.
//
// Events for unrelated files in the same directory are ignored — watching a
// directory is a means to an end, not a licence to reload whenever a
// neighbour changes.
func relevant(event fsnotify.Event, path, dir string) bool {
	name := filepath.Clean(event.Name)

	if name == path {
		return true
	}

	// A projected volume publishes through a swapped ..data symlink; the
	// file inside it never receives an event of its own.
	if filepath.Base(name) == kubernetesDataDir {
		return true
	}

	// The directory itself being replaced takes the file with it.
	return name == dir
}

func removedDirectory(event fsnotify.Event, dir string) bool {
	if filepath.Clean(event.Name) != dir {
		return false
	}

	return event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)
}

func (c *Config[T]) emitWatchError(err error) {
	failure := ReloadError{
		Err:        fmt.Errorf("filesystem watcher: %w", err),
		Stage:      StageWatch,
		Time:       time.Now(),
		Generation: c.generation.Load(),
		Source:     ReloadSourceFile,
	}

	if c.opts.logger != nil {
		c.opts.logger.Error(
			"dynamicconfig: filesystem watcher error",
			"error", err,
			"config_file", c.configFileUsed(),
		)
	}

	c.emitError(failure)
}
