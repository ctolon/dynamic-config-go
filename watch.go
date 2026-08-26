package dynamicconfig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ctolon/dynamic-config-go/internal/debounce"
	"github.com/ctolon/dynamic-config-go/internal/lifecycle"
	"github.com/fsnotify/fsnotify"
)

// projectedVolumePrefix is what every name a Kubernetes projected volume
// creates begins with: the staged data directory (..2026_08_26_10_00_00),
// the symlink swapped atomically over it (..data) and the temporary link
// used to perform the swap (..data_tmp).
//
// The configuration file in such a mount is a symlink into ..data, so it
// never receives an event of its own — the interesting events all name one
// of these instead. Nothing else in a configuration directory starts with
// two dots, which is what makes the prefix a safe signal rather than a
// guess. Matching the whole family rather than ..data alone also covers
// platforms whose watcher reports the staging of a swap but not the rename
// that completes it.
const projectedVolumePrefix = ".."

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

	files := c.configFilesUsed()
	if len(files) == 0 {
		return ErrNoConfigFile
	}

	for i := range files {
		files[i] = filepath.Clean(files[i])
	}

	return c.watch(ctx, files)
}

func (c *Config[T]) watch(ctx context.Context, files []string) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("dynamicconfig: watch: create watcher: %w", err)
	}

	defer func() { _ = watcher.Close() }()

	watched := newWatchSet(files)

	// Directories are watched rather than files. A file watch follows an
	// inode, and every realistic update — rename-into-place, a
	// projected-volume swap, delete-and-recreate — replaces the inode.
	// Watching the directory survives all three.
	//
	// A layered configuration usually has all its files in one directory,
	// so this is normally one watch; when it is not, each directory is
	// watched once however many files it holds.
	for _, dir := range watched.dirs {
		if err := watcher.Add(dir); err != nil {
			return fmt.Errorf("dynamicconfig: watch: watch directory %s: %w", dir, err)
		}
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
	if c.filesChangedSinceLoad() {
		debouncer.Trigger()
	}

	// Only now is the watch real: the directory is armed and the gap
	// between loading and watching has been closed. Status says so from
	// here, not from the moment the watcher slot was claimed.
	c.watching.Store(true)

	defer c.watching.Store(false)

	err = c.watchLoop(watchCtx, watcher, watched, debouncer)

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
	watched watchSet,
	debouncer *debounce.Debouncer,
) error {
	var (
		retryTimer *time.Timer
		retryC     <-chan time.Time
		lost       []string
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

			if watched.relevant(event) {
				debouncer.Trigger()
			}

			if dir, ok := watched.removedDirectory(event); ok {
				lost = append(lost, dir)

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

			var stillLost []string

			for _, dir := range lost {
				if err := watcher.Add(dir); err != nil {
					stillLost = append(stillLost, dir)
				}
			}

			lost = stillLost

			if len(lost) > 0 {
				scheduleRewatch()
			}

			// Whatever happened while a directory was gone was missed,
			// so reload unconditionally.
			debouncer.Trigger()
		}
	}
}

// watchSet is the files a watcher follows and the directories it has to
// watch to see them.
type watchSet struct {
	files map[string]struct{}
	dirs  []string
}

func newWatchSet(files []string) watchSet {
	set := watchSet{files: make(map[string]struct{}, len(files))}

	seen := make(map[string]struct{}, len(files))

	for _, path := range files {
		set.files[path] = struct{}{}

		dir := filepath.Dir(path)

		if _, ok := seen[dir]; ok {
			continue
		}

		seen[dir] = struct{}{}

		set.dirs = append(set.dirs, dir)
	}

	return set
}

// relevant decides whether an event concerns the configuration.
//
// Events for unrelated files in a watched directory are ignored — watching
// a directory is a means to an end, not a licence to reload whenever a
// neighbour changes.
func (w watchSet) relevant(event fsnotify.Event) bool {
	name := filepath.Clean(event.Name)

	if _, ok := w.files[name]; ok {
		return true
	}

	// A projected volume publishes through its own ..-prefixed machinery
	// rather than by writing to the file.
	if strings.HasPrefix(filepath.Base(name), projectedVolumePrefix) {
		return true
	}

	// A watched directory being replaced takes its files with it.
	for _, dir := range w.dirs {
		if name == dir {
			return true
		}
	}

	return false
}

// removedDirectory reports a watched directory disappearing, which is the
// one case a directory watch cannot recover from on its own.
func (w watchSet) removedDirectory(event fsnotify.Event) (string, bool) {
	if !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
		return "", false
	}

	name := filepath.Clean(event.Name)

	for _, dir := range w.dirs {
		if name == dir {
			return dir, true
		}
	}

	return "", false
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
