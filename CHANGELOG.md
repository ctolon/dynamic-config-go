# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/ctolon/dynamic-config-go/commits/main
