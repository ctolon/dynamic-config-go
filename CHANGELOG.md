# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

The first stable release. The API is frozen, every guarantee in the
documentation is a test, and the compatibility policy in
[docs/compatibility.md](docs/compatibility.md) says what will and will not
change from here.

### Breaking

- **`Config.Viper` is now the method `Viper()`.** A public field could be
  *assigned*, so anything holding a `Config` could swap the engine
  underneath a running reload; and a sealed configuration needed a way to
  say "no engine here" that was better than a nil field. Replace
  `cfg.Viper.X` with `cfg.Viper().X`. `Sealed()` is the explicit form of the
  nil check.

### Added

- **Sealed configurations.** `NewSealed` and `WrapSealed` build a `Config`
  whose `Viper()` returns nil: the engine cannot be read, mutated or
  replaced through it, so a configuration has exactly one public interface
  wherever it travels. This removes two problems outright — a second
  configuration API spreading through a codebase as `cfg.Viper().GetString`
  calls, and a goroutine reading the engine while a reload writes it, which
  Viper does not synchronise. `New` and `Wrap` are unchanged and still hand
  the engine back.

- **Layered configuration files.** `WithConfigFile` may now be given more
  than once, and `WithOptionalConfigFile` adds a file that is allowed to be
  absent. Files are read in order and later ones override earlier keys — a
  base, a separately mounted secret, an optional local override — producing
  one snapshot, one validator run and one generation however many files it
  came from. The watcher follows every file, in as many directories as they
  occupy, so a ConfigMap volume and a Secret volume work side by side. A
  file that is missing, unreadable or unparseable rejects the whole
  candidate rather than publishing half of it.

- `Status.ConfigFiles` lists every file the published snapshot was read
  from, in layering order. `Status.ConfigFile` remains the first of them.

- An [API surface test](api_test.go) holding the signatures the
  compatibility policy freezes, so changing one is a deliberate act.

- A [Kubernetes end-to-end test](scripts/e2e-kubernetes.sh) on a real kind
  cluster: a ConfigMap edit reaches a running pod, the pod reloads without
  restarting and keeps its UID, and an invalid edit is rejected while the
  pod goes on serving the last good configuration.

- A soak test, and a scheduled job that runs it for fifteen minutes:
  readers, writers, subscribers and reloads at once, checking that nothing
  accumulates with time.

- `docs/compatibility.md` — supported Go and Viper versions, the frozen API,
  and an honest filesystem matrix including what network filesystems do not
  guarantee. `docs/production.md` — reading patterns, what a reload does not
  reconfigure, validation guidance, shutdown ordering and alerting.

- Examples for [sealed configurations](examples/sealed) and for
  [multiple files](examples/multi-file), the latter showing both shapes:
  layers of one configuration in one instance, and separate configurations
  in separate instances.

### Fixed

- **A reload could publish after `Close` returned.** The close transition
  and the publication commit now share a one-line gate, giving them a total
  order: either the commit is first and the generation is then frozen, or
  the close is first and the reload returns `ErrClosed` without publishing.
  That error is not counted as a failed reload and is not delivered to error
  subscribers — a shutdown is not a verdict on the configuration.

- **A subscription racing `Close` could be retained.** Registries are now
  terminal: the decision to keep a handler is made inside the registry's own
  lock, so a closed configuration retains nothing and cannot keep whatever a
  handler captured — a pool, a cache, a whole service — reachable.

- **The dispatcher could start queued callbacks after `Stop`.** A single
  select over shutdown and work lets Go pick either when both are ready. The
  worker now checks for shutdown before it looks at the queue, so "queued
  work is abandoned" describes the implementation rather than the odds. The
  documented contract is the honest one: nothing starts after the worker
  observes the stop; a task already dequeued may finish.

- **A panicking validator took the process down.** A validator is
  application code on a recoverable path, so a panic in one is now recovered
  and turned into a rejected candidate — the same outcome as a returned
  error, with the same consequence, which is that the running configuration
  stays exactly where it was. During construction it becomes an error from
  the constructor.

### Changed

- Analysis tools (`staticcheck`, `govulncheck`) are pinned to explicit
  versions, and the third-party release action is pinned by commit SHA. A
  floating tool version can turn `main` red without a change to this
  repository, and a build that is not reproducible cannot tell you whether
  your own change broke something.

## [0.1.1] - 2026-08-26

### Fixed

- `Status().Watching` now reports an *established* watch rather than a
  claimed one. It used to become true the moment `Watch` took the watcher
  slot, which is before the directory is armed — so a caller that waited
  for it could act in a window where changes were not yet being observed.
- The startup check that closes the gap between loading and watching now
  notices a mode change. It compared size and modification time, and a
  `chmod` moves neither, so a file made unreadable in that window went
  unreported until the next write.
- Kubernetes projected-volume detection now matches the whole `..`-prefixed
  family — `..data`, `..data_tmp` and the staged `..<timestamp>` directory —
  rather than `..data` alone. Every name the mechanism uses starts with two
  dots and nothing else in a configuration directory does, so this also
  covers platforms whose watcher reports the staging of a swap but not the
  rename that completes it.

## [0.1.0] - 2026-08-26

### Added

The first release of the library, covering the whole of the intended v1
surface. Nothing here is staged for later: the guarantees only mean
something together.

**Core**

- `Config[T]`, with the Viper instance exposed as a public field.
- `New` and `Wrap`, both performing a fail-fast initial load.
- `Current()`: lock-free, allocation-free, never nil after construction.
- `Reload(ctx)`: the same transaction the watcher runs, serialised and
  context-aware.
- Atomic publication with last-known-good semantics — a rejected candidate
  never disturbs the published snapshot.
- Validation through `Validator[T]`, with no validation-framework
  dependency.

**Watching**

- `Watch(ctx)`, blocking, one watcher per `Config`.
- Debouncing with a bounded coalescing state machine, so no event rate can
  grow a queue.
- Atomic replacement (rename-into-place), deletion, re-creation and
  permission failures all handled without losing the running configuration.
- Kubernetes projected volumes: a `..data` symlink swap is recognised as an
  update to the configuration file.
- The load/watch gap closed by a stamp check at watcher startup, without
  republishing an unchanged file.

**Events and lifecycle**

- `Subscribe` and `SubscribeErrors`, with idempotent `Subscription` handles.
- Ordered asynchronous dispatch on a bounded queue; publication never waits
  for a subscriber.
- Callback panic isolation, reported as a `StageCallback` error.
- `Close()`: idempotent, deterministic, bounded even when a subscriber is
  not.
- A lifecycle state machine covering initialising, ready, watching, closing
  and closed.

**Observability**

- `Status()`, `Generation()` and `ReloadCount()`, carrying counters,
  timestamps and state — never configuration values.
- Optional `log/slog` integration, which never logs values.

**Options**

- `WithConfigFile`, `WithValidator`, `WithDebounce`, `WithEventBuffer`,
  `WithLogger`, `WithDecodeOption`, `WithAllowMissingFile`,
  `WithViperSetup`. Invalid values are rejected at construction rather than
  silently normalised.

**Quality**

- Unit, integration, concurrency, fuzz and benchmark suites.
- Everything runs under the race detector in CI, across Linux, macOS and
  Windows.
- `Current()`'s zero-allocation property asserted by a test, not only
  measured by a benchmark.
- Documentation covering design, concurrency, reloading, Kubernetes,
  security, migration from Viper, and troubleshooting.

[Unreleased]: https://github.com/ctolon/dynamic-config-go/compare/v0.1.1...HEAD
[0.1.0]: https://github.com/ctolon/dynamic-config-go/releases/tag/v0.1.0
[0.1.1]: https://github.com/ctolon/dynamic-config-go/compare/v0.1.0...v0.1.1
