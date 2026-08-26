# Design

## The boundary

One sentence decides most of what is in this package and what is not:

> Viper owns configuration. `dynamic-config-go` owns safe publication.

Viper is very good at the part that is genuinely tedious: finding a file,
recognising its format, parsing YAML and JSON and TOML, layering defaults
under it and environment variables over it, resolving aliases. Rewriting any
of that would cost a great deal of maintenance and produce nothing an
application could not already have.

What Viper does not do is answer the question every long-running service
eventually asks:

> How do I turn mutable configuration state into a validated typed snapshot
> while a hundred goroutines are reading it?

The answer is usually written once per service, slightly differently each
time:

```go
var cfg Config
var mu sync.RWMutex

viper.OnConfigChange(func(event fsnotify.Event) {
    var next Config

    if err := viper.Unmarshal(&next); err != nil {
        log.Println(err)
        return
    }

    mu.Lock()
    cfg = next
    mu.Unlock()
})
```

That sketch has no validation, takes a lock on every read, publishes
whatever parsed, and reloads several times per save. Each of those is a
decision, and each of them is what this package standardises.

### What each side owns

| Viper | dynamic-config-go |
| --- | --- |
| configuration sources | the typed `T` |
| file parsing and format detection | initial publication |
| environment variables, defaults, aliases | validation |
| file discovery | atomic snapshot publication |
| raw key access (`Get*`, `Set`) | last-known-good semantics |
| | reload serialisation |
| | debouncing and coalescing |
| | generations |
| | subscriptions and callback safety |
| | lifecycle and status |

## Why Viper is reachable — and why it can also be sealed

Viper is part of the contract, not an implementation detail. Hiding it
entirely and re-exporting the parts people need would produce a second
configuration DSL, then a stream of requests to widen it, and eventually a
wrapper around `RegisterAlias` that is subtly not `RegisterAlias`.

But leaving it reachable has a cost that is just as real. Viper has no
internal locking, so a runtime read racing a reload is a data race; and
`cfg.Viper().GetString` is an API that spreads, quietly turning a typed
configuration back into a stringly-typed one.

Both are true, so both shapes exist:

|                  | engine reachable | engine sealed |
| ---------------- | ---------------- | ------------- |
| **built for you** | `New`            | `NewSealed`   |
| **your instance** | `Wrap`           | `WrapSealed`  |

Sealing is not a lesser mode. It is the same machinery with one route
removed: `Viper()` returns nil, so nothing downstream of the `Config` can
read, mutate or replace the engine. The engine is still fully configurable
during construction, through the options and `WithViperSetup`, which is the
only moment at which configuring it is safe anyway.

`WrapSealed` is the one that pays off at a package boundary:

```go
func Load() (*dynamicconfig.Config[AppConfig], error) {
    v := viper.New()
    v.SetConfigFile("/etc/myapp/config.yaml")
    v.SetEnvPrefix("MYAPP")
    v.AutomaticEnv()

    return dynamicconfig.WrapSealed[AppConfig](v, dynamicconfig.WithValidator(validate))
}
```

The local `v` goes out of scope and every caller gets a configuration with
one public interface and no way behind it.

### Why `Viper()` is a method

It started as a public field, which read slightly better at a call site and
was wrong in two ways. A field can be *assigned* — anything holding the
`Config` could swap the engine underneath a running reload — and a sealed
`Config` would have had to express "no engine here" as a nil field, which is
a footgun rather than an answer.

A method can refuse. It is also the reason `Sealed()` exists: an explicit
question deserves an explicit answer rather than a nil check somebody has to
know to make.

## Why the API is this small

The public surface is eleven methods, four constructors and nine options.
Not because more would be hard to write, but because every addition is a
promise, and a configuration library's promises are load-bearing for
everything above it.

The option list stays short because `WithViperSetup` exists. Anything Viper
already does well is configured through Viper:

```go
dynamicconfig.WithViperSetup[AppConfig](func(v *viper.Viper) error {
    v.SetConfigName("app")
    v.AddConfigPath("/etc/myapp")
    v.SetEnvPrefix("MYAPP")
    v.AutomaticEnv()

    return nil
})
```

Mirroring `SetConfigName`, `AddConfigPath`, `SetEnvPrefix` and the rest as
options would double the surface and add nothing.

## Snapshots are pointers

`Current()` returns `*T`, and the immutability of what it points at is a
contract rather than a guarantee. Go offers no third option worth taking:

- Returning `T` by value looks safer and is not. A shallow copy shares every
  map, slice and pointer inside it, so the dangerous case — `Features["x"] =
  true` — is unchanged, and the cheap case now copies a struct on every
  read.
- Deep-copying on every read would make the contract real and destroy the
  property that makes `Current()` worth calling per request: an atomic load,
  no allocation, no contention.

So the pointer is returned, the contract is stated in the package
documentation, in the method documentation and here, and applications that
need a mutable value copy the parts they intend to change. A `Clone()`
helper is not in v1: generic deep cloning needs either reflection or
serialisation, and both have surprising behaviour on the kinds of types
configuration structs actually contain.

## What is deliberately absent

**Remote stores.** No Consul, etcd, Vault, Redis, NATS, S3 or HTTP
backend. Each is a network client, a retry policy, an authentication story
and a failure mode, and none of them makes the local hot-reload lifecycle
better.

**Fingerprinting.** A hash of the configuration is useful for observability
and dangerous by construction: hashing a generic `T` means serialising it,
and serialising it means walking fields that may be secrets. Absent from v1
on purpose.

**Content deduplication.** A filesystem event does not imply a changed
configuration, and comparing the decoded value would let identical content
skip a generation. `reflect.DeepEqual` on an arbitrary `T` is both expensive
and quietly wrong for some types, so the initial contract is the predictable
one: every successful reload publishes a new generation, even if the values
are equivalent. (The one exception is the check `Watch` makes at startup,
which compares the file's size and modification time — not its decoded
content — so that starting a watcher over an untouched file does not
manufacture a generation.)

**Automatic retries.** A configuration that does not validate will not
validate again in a second. Retrying it produces a log full of the same
error and nothing else. The next filesystem event is the retry.

**A polling fallback.** Network filesystems often deliver no events, and the
obvious answer — stat the file every few seconds — turns a library that is
idle between changes into one that is never idle. It would also be the
wrong default for the overwhelming majority of deployments, which are on a
local filesystem. Applications on NFS or SMB should reload from a signal or
a timer of their own, which is three lines and stays visible; see
[compatibility.md](compatibility.md#not-guaranteed).

**`WaitForGeneration`.** A method that blocks until a generation appears
would be convenient in tests and in control planes, and it would add a
condition variable, a wakeup path and a set of cancellation questions to a
library whose value is that it has none of those. Polling `Generation()` is
two lines in the test that needs it.

**A validation framework.** `func(*T) error` is the whole contract, so
go-playground/validator, ozzo-validation, generated code and a handful of
`if` statements are equally first-class, and the dependency graph gains
nothing.

**Metrics and tracing dependencies.** `Status()` is a struct of counters. An
application exports it through whatever it already uses.

## Dependencies

Direct: Viper, and fsnotify — which Viper already depends on, so neither is
a module an adopting application did not already have. Nothing for errors,
logging, worker pools, retries, validation, metrics, lifecycle or
debouncing — the standard library covers all of them, and each avoided
dependency is one an adopting application does not have to audit.

Logging is optional and, when enabled, uses `log/slog` from the standard
library.

## Compatibility policy

Lives in [compatibility.md](compatibility.md): the signatures frozen at 1.0,
the behavioural questions answered for good, the supported Go and Viper
versions, and the filesystem matrix.

The short version: adding a field to `Status`, an option, or a constructor
is a minor release. Changing what `Current()` may return is not a thing that
happens.
