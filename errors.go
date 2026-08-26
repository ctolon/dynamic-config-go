package dynamicconfig

import "errors"

// The sentinel errors the package returns. Callers should test with
// errors.Is rather than comparing strings; every error the package returns
// wraps its cause with %w, so the chain stays intact.
var (
	// ErrClosed is returned by Reload and Watch after Close.
	ErrClosed = errors.New("dynamicconfig: config closed")

	// ErrAlreadyWatching is returned by a second concurrent Watch on the
	// same Config. One watcher per Config keeps the lifecycle
	// unambiguous; see doc.go.
	ErrAlreadyWatching = errors.New("dynamicconfig: watcher already running")

	// ErrNoSnapshot reports that no configuration snapshot has been
	// published. A successful New or Wrap always publishes one, so this
	// cannot happen through the documented API; it exists so that the
	// internal checks that assert that invariant have something to return
	// instead of panicking.
	ErrNoSnapshot = errors.New("dynamicconfig: no configuration snapshot")

	// ErrNoConfigFile is returned by Watch when the Viper instance has no
	// configuration file to watch — an environment-only or
	// defaults-only configuration has nothing on disk to react to.
	ErrNoConfigFile = errors.New("dynamicconfig: no configuration file to watch")

	// ErrInvalidOption is returned by New and Wrap when an option carries
	// a value that cannot be used, such as a negative debounce. Options
	// are validated at construction rather than normalised silently.
	ErrInvalidOption = errors.New("dynamicconfig: invalid option")
)

// errCallbacksOutstanding is returned by Close when a subscriber callback
// was still running when the bounded shutdown wait expired. It is not part
// of the sentinel set: there is nothing a caller can usefully branch on,
// only something an operator should see in a log.
var errCallbacksOutstanding = errors.New(
	"dynamicconfig: close: subscriber callback still running after shutdown timeout",
)
