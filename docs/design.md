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

## Why Viper is public

```go
type Config[T any] struct {
    Viper *viper.Viper
    // ...
}
```

Viper is part of the contract, not an implementation detail. The
alternative — hiding it and re-exporting the parts people need — produces a
second configuration DSL, and then a stream of requests to widen it, and
then a wrapper around `RegisterAlias` that is subtly not `RegisterAlias`.

It is a field rather than an embedded type deliberately. Embedding would put
`cfg.Get`, `cfg.Set` and `cfg.Unmarshal` next to `cfg.Current`, invite
method-name collisions, and blur exactly the distinction the package exists
to draw. `cfg.Viper.GetString(...)` reads as what it is: a question for
Viper, not for the application's configuration.

## Why the API is this small

The public surface is nine methods, two constructors and eight options. Not
because more would be hard to write, but because every addition is a
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

**A validation framework.** `func(*T) error` is the whole contract, so
go-playground/validator, ozzo-validation, generated code and a handful of
`if` statements are equally first-class, and the dependency graph gains
nothing.

**Metrics and tracing dependencies.** `Status()` is a struct of counters. An
application exports it through whatever it already uses.

## Dependencies

Direct: Viper, and fsnotify, which Viper already depends on. Nothing for
errors, logging, worker pools, retries, validation, metrics, lifecycle or
debouncing — the standard library covers all of them, and each avoided
dependency is one an adopting application does not have to audit.

Logging is optional and, when enabled, uses `log/slog` from the standard
library.

## Compatibility policy

Before 1.0 the API may still move. From 1.0 the following are stable
contracts, and breaking any of them requires a major version:

- `Config[T]`, `New`, `Wrap`, `Current`, `Reload`, `Watch`, `Close`;
- validation semantics: read, decode, validate, publish, in that order;
- last-known-good: a rejected candidate never disturbs the published
  snapshot;
- subscription semantics, including bounded best-effort delivery;
- error wrapping: every returned error wraps its cause, and the sentinels
  answer `errors.Is`.

Adding a field to `Status` or an option to the constructor is a minor
release. Changing what `Current()` may return is not a thing that happens.
