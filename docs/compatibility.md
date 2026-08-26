# Compatibility

What this library supports, what it promises not to break, and where its
guarantees stop being its own.

## Versioning

[Semantic versioning](https://semver.org/). Below 1.0 the API may still
move, and a minor version is where that happens; the changelog says what
moved and why.

### Frozen at 1.0

From 1.0 these are contracts, and breaking one requires a major version:

**Signatures**

```go
New[T](...Option[T]) (*Config[T], error)
NewSealed[T](...Option[T]) (*Config[T], error)
Wrap[T](*viper.Viper, ...Option[T]) (*Config[T], error)
WrapSealed[T](*viper.Viper, ...Option[T]) (*Config[T], error)

(*Config[T]).Current() *T
(*Config[T]).Viper() *viper.Viper
(*Config[T]).Sealed() bool
(*Config[T]).Reload(context.Context) error
(*Config[T]).Watch(context.Context) error
(*Config[T]).Close() error
(*Config[T]).Subscribe(ChangeHandler[T]) Subscription
(*Config[T]).SubscribeErrors(ErrorHandler) Subscription
(*Config[T]).Status() Status
(*Config[T]).Generation() uint64
(*Config[T]).ReloadCount() uint64
```

and the nine options: `WithConfigFile`, `WithOptionalConfigFile`,
`WithValidator`, `WithDebounce`, `WithEventBuffer`, `WithLogger`,
`WithDecodeOption`, `WithAllowMissingFile`, `WithViperSetup`.

An [API surface test](../api_test.go) holds these signatures, so changing
one is a deliberate act rather than an accident noticed by a user.

**Behaviour**

| Question | Answer, frozen at 1.0 |
| --- | --- |
| Does `Current()` stay pointer-based? | Yes. A copy would share maps and slices anyway, and deep-copying would end the zero-allocation read. |
| Is the initial generation always 1? | Yes. |
| Does `SuccessfulReloads` exclude the initial load? | Yes: `Generation == SuccessfulReloads + 1`, always. |
| Does `Watch` block? | Yes. The goroutine belongs to the application. |
| Is one watcher per `Config` guaranteed? | Yes; a second returns `ErrAlreadyWatching`. |
| Is subscriber delivery best-effort? | Yes, permanently. Publication is reliable; delivery is bounded. |
| Does `Current()` stay usable after `Close()`? | Yes, and frozen. |
| Does `Close()` freeze the generation? | Yes. Once close begins, nothing is published again. |
| Is `Viper()` nil for a sealed `Config`? | Yes. |
| Do layered files merge later-over-earlier? | Yes, in the order the options named them. |
| Does one reload publish one snapshot however many files it read? | Yes, and one generation. |
| Do errors keep wrapping their cause? | Yes; `errors.Is` and `errors.As` work through every returned error. |

Adding a `Status` field, an option, or a constructor is a minor release.
Changing what `Current()` may return is not a thing that happens.

### Withdrawing a version

A published version cannot be unpublished: the module proxy caches it the
first moment anyone fetches it, and a moved tag stays wrong for everyone who
already has the old one. A broken release is withdrawn with a `retract`
directive in a *later* release, and fixed by cutting the next patch.

## Go

| | |
| --- | --- |
| Minimum | Go 1.24 |
| Tested | Go 1.24 and current stable, on Linux, macOS and Windows |

The minimum tracks what the library actually needs — generics with the
ergonomics this API depends on, `log/slog`, and `testing.B.Loop` in the
benchmarks. It rises only in a minor release before 1.0, and only in a major
release afterwards, unless a security fix is unavailable on the old
minimum. A rise is always a changelog entry.

## Viper

The library builds on Viper v1 and uses a small, stable part of its surface:
`ReadInConfig`, `Unmarshal`, `ConfigFileUsed`, `SetConfigFile`, and whatever
the application configures itself.

CI tests the dependency graph in `go.mod` — the minimum this module selects
— and Go's minimum version selection means an application that requires a
newer Viper v1 gets that newer one instead. Both are expected to work;
the pinned graph is what the tests prove.

The library does not vendor Viper's behaviour or work around its bugs. One
thing it deliberately does *not* use is `viper.WatchConfig`; the reasoning
is in [migration-from-viper.md](migration-from-viper.md#replacing-viperwatchconfig).

## Filesystems

Watching is fsnotify's, and fsnotify's guarantees are the operating
system's. This is where the library's promises stop being its own, so it is
worth being blunt rather than reassuring.

### Tested

- Linux local filesystems (ext4, xfs, btrfs, tmpfs)
- Linux container overlay filesystems, which is what CI runs on
- macOS local filesystems
- Windows local filesystems
- Kubernetes ConfigMap and Secret projected volumes, on Linux

Each of these is exercised by the integration suite on every push: direct
writes, rename-into-place, deletion, re-creation, permission failures and —
on Linux — the projected-volume symlink swap.

### Best effort

- bind mounts
- symlinked configuration paths that point outside the watched directory
- container runtimes other than the one CI uses

These usually work. They are not covered by a test, so a report about one is
a bug report worth filing rather than a promise being broken.

### Not guaranteed

- NFS
- SMB/CIFS
- FUSE filesystems
- other network and distributed filesystems

Inotify and kqueue watch a local kernel's view of a filesystem. A change
written by another host does not produce a local event, so a watcher may
never fire — not slowly, not eventually, but never. No library can invent
those events, and one that claims to support network filesystems is either
polling or wrong.

If configuration lives on one, do not rely on `Watch`. Reload deliberately:

```go
// A signal, from a deployment step or an operator.
for range hangups {
    if err := cfg.Reload(ctx); err != nil {
        slog.Error("reload failed", "error", err)
    }
}
```

or on a timer. `Reload` is the same transaction the watcher runs, so
last-known-good, validation and atomic publication all still apply.

### macOS and symlink swaps

kqueue follows a watched symlink to its target, so replacing the link
itself produces no event. This affects the Kubernetes projected-volume
pattern, which is a Linux mechanism and is tested there; it is called out
here because the same shape can appear in a hand-rolled symlink deployment
on a developer's Mac.

## Performance

Absolute numbers depend on the machine and are not part of any contract.
The shape of them is:

| Operation | Guarantee |
| --- | --- |
| `Current()` | O(1), zero allocations, no locks |
| `Status()` | O(1), no decoding, no allocation |
| `Reload()` | dominated by file I/O, decode and validation |
| Memory per snapshot | O(size of decoded configuration) |
| Watch queue | bounded |
| Subscriber queue | bounded |

The zero-allocation property of `Current()` is a test
([`TestCurrentDoesNotAllocate`](../benchmarks/current_test.go)) rather than
a benchmark line somebody reads occasionally. Latency is tracked with
`make bench` and compared across releases with `benchstat`; CI does not
fail a build on a latency number, because a shared runner's variance would
make that a coin toss rather than a gate.

## Dependencies

Two modules, and neither is one a Viper application did not already have:

| Module | Why | Already in a Viper app's graph |
| --- | --- | --- |
| `github.com/spf13/viper` | the configuration engine | yes, by definition |
| `github.com/fsnotify/fsnotify` | the watcher | yes — Viper depends on it |

### Why fsnotify is listed as direct

Because this package imports it: the watcher is its own, for the correctness
reasons in
[migration-from-viper.md](migration-from-viper.md#replacing-viperwatchconfig)
and because a layered configuration watches several files in several
directories while Viper's watcher follows exactly one.

`go mod tidy` marks a module direct when the main module imports it, so the
line cannot be removed while the import exists — deleting it by hand puts it
straight back on the next tidy. Nor would switching to Viper's own watcher
help: its API is `OnConfigChange(func(in fsnotify.Event))`, so using it
means importing fsnotify too.

None of this costs an application anything. Viper requires
`fsnotify v1.9.0`, this module requires the same v1.9.0, and Go's minimum
version selection resolves one copy. `go.sum` carries the identical two
lines whether the requirement is direct or indirect:

```text
github.com/fsnotify/fsnotify v1.9.0 h1:...
github.com/fsnotify/fsnotify v1.9.0/go.mod h1:...
```

Direct versus indirect is a statement about *who imports it*, not about
what is downloaded, built, vendored or audited.

Nothing is pulled in for errors, logging, worker pools, retries, validation,
metrics, lifecycle or debouncing. The standard library covers all of them,
and each avoided dependency is one an adopting application does not have to
audit.

Updates arrive as automated pull requests and are merged once CI proves
them. Analysis tools (`staticcheck`, `govulncheck`) are pinned to explicit
versions in the [Makefile](../Makefile), because a floating tool version can
turn `main` red without a single change to this repository.
