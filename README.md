# dynamic-config-go

[![CI](https://github.com/ctolon/dynamic-config-go/actions/workflows/ci.yml/badge.svg)](https://github.com/ctolon/dynamic-config-go/actions/workflows/ci.yml)
[![Security](https://github.com/ctolon/dynamic-config-go/actions/workflows/security.yml/badge.svg)](https://github.com/ctolon/dynamic-config-go/actions/workflows/security.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ctolon/dynamic-config-go.svg)](https://pkg.go.dev/github.com/ctolon/dynamic-config-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/ctolon/dynamic-config-go)](https://goreportcard.com/report/github.com/ctolon/dynamic-config-go)
[![Go 1.24+](https://img.shields.io/badge/go-1.24%2B-00ADD8)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Typed, validated, atomic hot reload for Viper.**

`dynamic-config-go` turns Viper configuration into validated typed snapshots
that can be replaced atomically at runtime, without interrupting concurrent
readers and without restarting the process.

```go
cfg, err := dynamicconfig.New[AppConfig](
    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
    dynamicconfig.WithValidator(validateConfig),
)
if err != nil {
    return fmt.Errorf("initialize configuration: %w", err)
}

defer cfg.Close()

go cfg.Watch(ctx)

// On every request, for the lifetime of the process:
current := cfg.Current()
```

`Current()` costs an atomic load and allocates nothing. It never returns
nil, never returns a half-decoded value, and never returns a configuration
that failed validation.

## What this is not

It is not a better Viper, and it is not a replacement for Viper. Viper keeps
its job — files, formats, defaults, environment variables, aliases, search
paths. This package owns the layer every long-running service ends up
writing on top of Viper anyway:

```text
Viper state ──► decode into T ──► validate ──► publish ──► Current()
                                      │
                                   invalid
                                      │
                                      └──► rejected, previous snapshot stays
```

That layer is usually written once per service, slightly differently each
time, with the synchronisation, the panic isolation, the debouncing and the
"what happens when the file is briefly half-written" question answered
somewhere between "carefully" and "not at all".

## Install

```bash
go get github.com/ctolon/dynamic-config-go
```

Go 1.24 or later, and one dependency you did not already have: none.

`go.mod` names Viper and fsnotify. Viper requires `fsnotify v1.9.0` itself
and this module requires the same version, so both are already in the graph
of any Viper application — one copy, one `go.sum` entry, nothing extra
downloaded, built or audited. fsnotify is listed as *direct* simply because
this package imports it: the watcher is its own rather than Viper's, for the
reasons in
[docs/migration-from-viper.md](docs/migration-from-viper.md#replacing-viperwatchconfig).
See [docs/compatibility.md](docs/compatibility.md#dependencies).

## Two shapes: open and sealed

A `Config` either lets you reach the Viper instance behind it or it does
not, and that is a construction-time choice:

|                  | engine reachable | engine sealed |
| ---------------- | ---------------- | ------------- |
| **built for you** | `New`            | `NewSealed`   |
| **your instance** | `Wrap`           | `WrapSealed`  |

**Open** hands the engine back through `cfg.Viper()`, so everything Viper
does well stays available and an existing Viper codebase can adopt this
package without rewriting anything:

```go
cfg, err := dynamicconfig.New[AppConfig](
    dynamicconfig.WithConfigFile[AppConfig]("config.yaml"),
)

cfg.Viper().GetString("server.host")   // Viper, as usual
```

**Sealed** returns nil from `cfg.Viper()`. The engine cannot be read,
mutated or replaced through the `Config`, so the configuration has exactly
one public interface:

```go
func Load() (*dynamicconfig.Config[AppConfig], error) {
    v := viper.New()
    v.SetConfigFile("/etc/myapp/config.yaml")
    v.SetEnvPrefix("MYAPP")
    v.AutomaticEnv()

    return dynamicconfig.WrapSealed[AppConfig](v,
        dynamicconfig.WithValidator(validateConfig),
    )
}
```

Every caller of `Load` gets a configuration and no way behind it. Sealing
removes two problems outright: a second configuration API spreading through
a codebase as `cfg.Viper().GetString` calls, and a goroutine reading the
engine while a reload writes it — which Viper does not synchronise, and this
package cannot synchronise on its behalf.

Neither shape is the lesser one. A sealed engine is still fully
configurable, during construction, through the options and `WithViperSetup`
— which is the only moment at which configuring it is safe anyway.

## More than one file

A configuration often arrives in pieces: a base checked into the repository,
a secret mounted separately, an optional local override. They are layers of
*one* configuration, so they belong in one instance:

```go
cfg, err := dynamicconfig.NewSealed[AppConfig](
    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/secrets/secret.yaml"),
    dynamicconfig.WithOptionalConfigFile[AppConfig]("config.local.yaml"),
    dynamicconfig.WithValidator(validateConfig),
)
```

Files are read in the order given and later ones override earlier keys. One
snapshot, one validator run — so a rule spanning two files has somewhere to
live — and one generation, however many files it came from. The watcher
follows every one of them, in as many directories as they occupy, which is
what makes a ConfigMap volume and a Secret volume work side by side.

A file added with `WithOptionalConfigFile` may be absent. Anything else
missing, unreadable or unparseable rejects the whole candidate rather than
publishing half of it — a deleted secret file must never quietly demote a
service to its base configuration.

When the files are *different configurations* that merely share a process —
owned by different teams, changing on their own schedules, one being broken
should not hold up the other — give each its own instance and its own
struct. [examples/multi-file](examples/multi-file) shows both shapes side by
side.

## Current is not the engine

In an open configuration:

```go
cfg.Viper().GetInt("server.port")  // Viper's current, mutable state
cfg.Current().Server.Port          // the last snapshot that decoded and validated
```

They can disagree, and when they do, `Current()` is the one the application
should believe:

```go
cfg.Viper().Set("server.port", -1)

cfg.Viper().GetInt("server.port")  // -1, immediately
cfg.Current().Server.Port          // still 8080
```

Setting a value on Viper does not publish it. A reload does — and a reload
that does not validate publishes nothing:

```go
cfg.Viper().Set("feature.enabled", true)

if err := cfg.Reload(ctx); err != nil {
    return err
}
```

**The same is true of the environment.** Viper reads environment variables
when asked, so a variable that changes inside the running process is picked
up by the next reload — and nothing triggers that reload, because only files
produce events. Call `Reload(ctx)` when the environment changes underneath
you. (A variable changed by an orchestrator *outside* the process does not
reach it at all until it restarts; that is the operating system, not this
library.)

## Guarantees

After a successful construction:

- `Current()` is never nil, and construction publishes exactly generation 1.
- Only configurations that were read, decoded **and** validated are ever
  published.
- A failed reload never changes what `Current()` returns — not to nil, not
  to a half-decoded value, not to the rejected candidate.
- Readers see snapshot N or snapshot N+1, never a mixture.
- Generation advances exactly once per publication and never decreases.
- Reloads are serialised; readers never take a lock.
- Once `Close()` begins, nothing is ever published again.
- `Current()` stays readable, and frozen, after `Close()`.
- Subscriber callbacks run outside every lock the package holds, so a
  handler may call back into the `Config`.
- A panicking subscriber costs its own callback and nothing else.
- A panicking validator rejects its candidate and nothing else.
- `Close()` is idempotent and every internal queue is bounded.
- No configuration value is ever logged by this package.

Each of these is a test, not an aspiration. The full list, with the
reasoning, is in [docs/concurrency.md](docs/concurrency.md).

## Last-known-good

This is the guarantee worth paying for. A service running on a good
configuration cannot be talked out of it by a bad one:

```text
generation 3 (good)
     │
     ├── file changes
     ├── read, decode, validate
     ├── validation fails
     │
     └── generation 3 still current  ← never nil, never the bad value
```

A half-written file, a deleted ConfigMap, a `chmod 000`, a YAML document
with a typo in it, a validator that panics: all of them are reported to the
error subscribers and none of them disturbs the running configuration.
Repair the file and the next event publishes it.

Startup is the deliberate exception. Construction fails on a configuration
it cannot understand, because there is no last-known-good to fall back to
and a service should not start on configuration it did not read.

## Hot reload

```go
go func() {
    if err := cfg.Watch(ctx); err != nil && !errors.Is(err, context.Canceled) {
        slog.Error("configuration watcher stopped", "error", err)
    }
}()
```

`Watch` blocks, on purpose: the goroutine's lifetime belongs to the
application and is visible at the call site. It handles the cases that
actually occur in production —

- bursts of events from one logical write, folded into one reload;
- atomic replacement (write a temporary file, rename it over the target),
  which is what editors and deployment tools do;
- Kubernetes projected volumes, where an update is a symlink swap and the
  file itself is never written to;
- deletion, which reports an error and keeps the last good snapshot, and
  re-creation, which publishes again;
- the gap between loading and starting to watch, which is otherwise a
  silently missed update.

## Reacting to changes

```go
sub := cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
    slog.Info("configuration reloaded", "generation", change.Generation)
})

defer sub.Unsubscribe()

cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
    slog.Error("reload rejected", "stage", e.Stage, "error", e.Err)
})
```

Publication is reliable; delivery is best-effort and bounded. A subscriber
can be slow without slowing a reload down, and a subscriber that needs
authoritative state should call `Current()` rather than assume it saw every
event.

A new snapshot does not reconfigure anything built from the old one — a
connection pool, an HTTP client, a TLS configuration keep whatever they were
constructed with. Rebuilding them is the application's job, and a subscriber
is where it usually starts. See [docs/production.md](docs/production.md).

## Health and metrics

```go
status := cfg.Status()
```

Counters, timestamps and state — never values, so it is safe to expose:

```text
dynamic_config_generation              Status.Generation
dynamic_config_reload_success_total    Status.SuccessfulReloads
dynamic_config_reload_failure_total    Status.FailedReloads
dynamic_config_events_dropped_total    Status.DroppedEvents
dynamic_config_last_success_timestamp  Status.LastSuccess
```

There is no Prometheus dependency and no OpenTelemetry dependency. A
configuration library has no business enlarging an application's telemetry
graph.

## Migrating from Viper

Wrapping an existing instance is a first-class path:

```go
v := viper.New()
v.SetConfigFile("config.yaml")
v.SetEnvPrefix("MYAPP")
v.AutomaticEnv()

cfg, err := dynamicconfig.Wrap[AppConfig](v, dynamicconfig.WithValidator(validate))
```

Everything already configured on that instance keeps working. Application
code moves from a global `config` variable to `cfg.Current()`, and gets
validation and safe reloads on the way. See
[docs/migration-from-viper.md](docs/migration-from-viper.md).

## Performance

Measured on an Intel i7-14700F, Go 1.26, `make bench`:

| Operation                   |        Cost |     Allocations |
| --------------------------- | ----------: | --------------: |
| `Current()`                 |    0.53 ns  |               0 |
| `Current()` (parallel)      |    0.03 ns  |               0 |
| `Current()` + field reads   |    1.31 ns  |               0 |
| `Status()`                  |   12.64 ns  |               0 |
| `Reload()`                  |    ~61 µs   |             400 |
| `Reload()` with validation  |    ~59 µs   |             400 |

Reload is dominated by Viper reading and parsing the file, which is the
correct place for the cost to be: it happens when a file changes, not when a
request arrives. The zero-allocation property of `Current()` is asserted by
a test, not just measured by a benchmark. Absolute numbers are not part of
the contract; the shape of them is — see
[docs/compatibility.md](docs/compatibility.md).

## Filesystems

Watching is fsnotify's, and fsnotify's guarantees are the operating
system's:

| | |
| --- | --- |
| **Tested** | Linux, macOS and Windows local filesystems; container overlay filesystems; Kubernetes projected volumes on Linux |
| **Best effort** | bind mounts, symlinked paths, other container runtimes |
| **Not guaranteed** | NFS, SMB/CIFS, FUSE and other network filesystems |

A network filesystem often delivers no events at all, and no library can
invent them. On one, reload from a signal or a timer instead of watching.
The full policy is in [docs/compatibility.md](docs/compatibility.md).

## Examples

| Example | What it shows |
| ------- | ------------- |
| [basic](examples/basic) | Load a file, read a snapshot |
| [multi-file](examples/multi-file) | Layered files in one instance, and separate instances per configuration |
| [sealed](examples/sealed) | A configuration with no way behind it |
| [validation](examples/validation) | A validator, and why Viper's state and the snapshot differ |
| [watch](examples/watch) | Hot reload, subscriptions, SIGHUP, graceful shutdown |
| [environment](examples/environment) | Defaults and environment variables, file optional |
| [http-server](examples/http-server) | One snapshot per request, `Status` on `/healthz` |
| [kubernetes](examples/kubernetes) | ConfigMap volume, probes, manifests |

## Documentation

- [docs/design.md](docs/design.md) — the boundary with Viper, and why the API is this small
- [docs/concurrency.md](docs/concurrency.md) — what is safe, what is not, and the invariants
- [docs/reloading.md](docs/reloading.md) — the reload transaction, debouncing, event delivery
- [docs/production.md](docs/production.md) — running it in a service: patterns, shutdown, observability
- [docs/kubernetes.md](docs/kubernetes.md) — ConfigMaps, Secrets, projected volumes, `subPath`
- [docs/security.md](docs/security.md) — secrets, logging, the threat model
- [docs/compatibility.md](docs/compatibility.md) — supported Go, Viper, platforms and filesystems
- [docs/migration-from-viper.md](docs/migration-from-viper.md) — incremental adoption
- [docs/troubleshooting.md](docs/troubleshooting.md) — when reload does not happen

## Status

Pre-1.0. The API is what it intends to be at 1.0, and the guarantees above
are tested rather than asserted, but the version number stays below one
until the contracts have survived real use. See [CHANGELOG.md](CHANGELOG.md)
and the compatibility policy in
[docs/compatibility.md](docs/compatibility.md).

## Contributing

Bug reports, and pull requests that keep the scope narrow, are welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md) for what the project is trying to be and
what a change needs before it lands, [RELEASING.md](RELEASING.md) for how a
version is cut, and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

```bash
make check   # everything CI runs
```

Security issues go through
[the advisory form](https://github.com/ctolon/dynamic-config-go/security/advisories/new),
not the issue tracker. See [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
