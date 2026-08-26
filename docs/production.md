# Running this in a service

The library's guarantees cover what it publishes. This is about the part
around it: how application code should read configuration, what a reload
does *not* do, how to shut down, and what to watch in production.

## The pattern

Read one snapshot per unit of work — a request, a job, a message — and use
that value throughout:

```go
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
    current := s.cfg.Current()

    ctx, cancel := context.WithTimeout(r.Context(), current.Timeout)
    defer cancel()

    s.serve(ctx, w, current.Server.Host, current.Server.Port)
}
```

Not this:

```go
timeout := s.cfg.Current().Timeout       // a reload can land here
host := s.cfg.Current().Server.Host      // and now these two disagree
```

`Current()` is cheap enough to call per request — an atomic load, no
allocation — and calling it once per *request* rather than once per *field*
is what makes a request internally consistent. Two calls can straddle a
reload and mix two generations.

The same rule at longer range: a snapshot cached in a struct field at
startup will never change. If a component should follow configuration, give
it the `*Config` and let it read; if it should not, that is a decision worth
making explicitly rather than by accident.

## What a reload does not do

Publishing a new snapshot changes what `Current()` returns. It does not
touch anything the application already built from the old one:

- a `*sql.DB` and its pool limits
- an HTTP or gRPC client, and its transport
- a Kafka, Redis or NATS client
- an open listener, and the port it is bound to
- a `tls.Config` already handed to a server
- a worker pool and its size

Those keep whatever they were constructed with. Rebuilding them is the
application's job, and a subscriber is where it usually starts:

```go
cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
    if change.Previous == nil {
        return
    }

    if change.Current.Database.MaxOpen != change.Previous.Database.MaxOpen {
        db.SetMaxOpenConns(change.Current.Database.MaxOpen)
    }
})
```

Two things to keep in mind while doing it. Handlers run one at a time on a
single goroutine, so slow reconstruction belongs on a goroutine or a queue
of the application's own. And delivery is best-effort: a handler that must
not miss a change should compare against `Current()` rather than assume it
saw every event.

Some values simply cannot be reloaded — a listen address, once bound, is
bound. Validate those at startup and treat a change to them as requiring a
restart. It is usually worth saying so in the validator's error message.

## Validation

Without a validator the library still guarantees atomic publication and
last-known-good on a decode failure — but "good" then means only "parsed",
and a `port: 0` sails through. A validator is what makes the guarantee worth
something, and it is where most of this library's value is realised.

Worth covering:

- numeric ranges and bounds (ports, pool sizes, timeouts)
- required values that must not be empty
- relationships between fields (`max_idle <= max_open`)
- mutually exclusive options
- enum-like strings (log levels, modes, TLS policies)
- URL and address syntax
- durations that must be positive, or below a ceiling

Keep validators **local and deterministic**. They run synchronously inside
the reload transaction, so this is a mistake:

```go
func validate(c *AppConfig) error {
    return pingDatabase(c.Database.DSN)   // don't
}
```

It makes reload latency unbounded, and it couples whether a configuration is
*acceptable* to whether a dependency is *up* — so a database blip becomes a
configuration outage. Check reachability in a health check, not in a
validator.

Validators must not call `Reload` or `Close`: both wait on the transaction
the validator is running inside. A panic in one is recovered and turned into
a rejected candidate, so a mistake in a validation rule cannot take the
process down.

And validator errors are logged when a logger is configured, so they must
not contain configuration values:

```go
return fmt.Errorf("database.password is too short")               // good
return fmt.Errorf("bad password %q", c.Database.Password)         // don't
```

## Startup

Construction fails on configuration it cannot read, decode or validate, and
that is the intended behaviour: there is no last-known-good to fall back to,
and startup is the cheapest moment to find out.

```go
cfg, err := dynamicconfig.New[AppConfig](
    dynamicconfig.WithConfigFile[AppConfig](path),
    dynamicconfig.WithValidator(validate),
    dynamicconfig.WithLogger[AppConfig](logger),
)
if err != nil {
    logger.Error("initialize configuration", "error", err)
    os.Exit(1)
}
```

In an orchestrator this is what turns a bad configuration into a visible
crash loop rather than a service running on defaults nobody chose. Note the
asymmetry with runtime: a bad configuration at *startup* stops the process,
while a bad configuration at *reload* is rejected and survived.

## Watching

`Watch` blocks, so the goroutine belongs to the application:

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

go func() {
    if err := cfg.Watch(ctx); err != nil &&
        !errors.Is(err, context.Canceled) &&
        !errors.Is(err, dynamicconfig.ErrClosed) {
        logger.Error("configuration watcher stopped", "error", err)
    }
}()
```

Do not discard that error. A watcher that returns early — a directory that
vanished, a watch that could not be established — is a service that has
quietly stopped following its configuration, and the returned error is the
only place it is reported.

`Status().Watching` is the same fact in polled form, and it means an
*established* watch: the directory is armed and changes are being observed.

Manual reload works alongside it, and needs no support from the library:

```go
hangups := make(chan os.Signal, 1)
signal.Notify(hangups, syscall.SIGHUP)

go func() {
    for range hangups {
        if err := cfg.Reload(ctx); err != nil {
            logger.Error("manual reload failed", "error", err)
        }
    }
}()
```

## Shutdown

```go
defer cfg.Close()
```

In order, `Close`:

1. **freezes publication** — a reload already in flight either committed
   before the close or finds the configuration closed at its commit and
   returns `ErrClosed`; nothing is ever published afterwards;
2. **stops the watcher**, whether or not its context was cancelled;
3. **stops the dispatcher**, waiting up to five seconds for a callback that
   is already running and abandoning what is merely queued;
4. **drops every subscription**, retaining no handler — so nothing a
   subscriber captured is kept alive by a closed configuration.

Afterwards `Current()` keeps returning the final snapshot, frozen, for as
long as anything holds the `Config`. Work still in flight during shutdown
does not have its configuration pulled out from under it. `Reload` and
`Watch` return `ErrClosed`.

`Close` is idempotent and safe from any goroutine. It returns an error only
when a subscriber callback was still running when the five-second wait
expired; the configuration is closed either way, and the message means some
handler is blocking for longer than it should be.

## Observability

`Status()` carries counters, timestamps and state — never configuration
values — so it is safe to expose from a health or debug endpoint and to feed
straight to a metrics exporter:

```text
dynamic_config_generation              Status.Generation
dynamic_config_reload_success_total    Status.SuccessfulReloads
dynamic_config_reload_failure_total    Status.FailedReloads
dynamic_config_events_dropped_total    Status.DroppedEvents
dynamic_config_watching                Status.Watching
dynamic_config_last_success_timestamp  Status.LastSuccess
dynamic_config_last_failure_timestamp  Status.LastFailure
```

Alerts worth having, in rough order of how often they earn their keep:

| Condition | What it means |
| --- | --- |
| `FailedReloads` rising while `Generation` is flat | The configuration on disk changed and the service is refusing it. It is still serving the last good one, so this is urgent but not an outage. |
| `Watching == false` while the process is up | The watcher stopped. Configuration changes are no longer being picked up, silently. |
| `LastFailure > LastSuccess` for longer than a deployment takes | A rejected change was never repaired. |
| `DroppedEvents` rising | A subscriber cannot keep up. Publication is unaffected; whatever that subscriber reconfigures is lagging. |
| `Generation` unexpectedly high | Something is rewriting the file in a loop — a templating sidecar, usually. |

Log the generation and the source, never the snapshot:

```go
cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
    logger.Info("configuration reloaded",
        "generation", change.Generation,
        "source", string(change.Source),
    )
})
```

`Change` has no `String` or `MarshalJSON` method precisely so that a change
event cannot be rendered into a log line by accident.

## Readiness and liveness

Readiness should stay **true** when a reload is rejected. The process is
still serving the last good configuration, and removing it from the load
balancer because somebody typed a bad value into a configuration file turns
a harmless mistake into an outage. Report the rejection through logs,
`Status().FailedReloads` and an alert.

Liveness has nothing to do with configuration at all: a service refusing a
bad reload is working exactly as designed.

```go
mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
    if cfg.Current() == nil {
        w.WriteHeader(http.StatusServiceUnavailable)

        return
    }

    w.WriteHeader(http.StatusOK)
})
```

## One instance or several

Two files, two different questions.

**Layers of one configuration** — a base, a secret, an optional override —
belong in one instance:

```go
dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/secrets/secret.yaml"),
dynamicconfig.WithOptionalConfigFile[AppConfig]("config.local.yaml"),
```

One snapshot, one validator run, one generation. Rules that span the files
have somewhere to live, and application code has one thing to read.

**Different configurations that share a process** — owned by different
teams, changing on their own schedules — belong in separate instances, each
with its own struct and validator. Then a broken telemetry file cannot hold
up an application reload, each has its own generation counter to watch, and
neither validator has to know the other exists.

The question to ask is whether a rule could ever span the two files. If it
could, they are layers. If it could not, they are separate configurations.

## Choosing a shape

Use a sealed configuration — `NewSealed` or `WrapSealed` — when the `Config`
will be passed to code that should depend on the configuration rather than
on the machinery behind it. It removes the possibility of
`cfg.Viper().GetString` spreading through the codebase as a second
configuration API, and the possibility of a goroutine reading the engine
while a reload writes it.

Use an open one when the application genuinely still uses Viper — during a
migration, or where some subsystem needs raw key access that the typed
struct does not cover. Then treat `cfg.Viper()` as construction-time state:

```go
// Startup, before anything can reload:
cfg.Viper().SetDefault("server.port", 8080)

// Runtime:
current := cfg.Current()   // and never cfg.Viper().GetInt(...)
```

The distinction is not stylistic. Viper does no internal locking, so a
runtime read racing a reload is a data race the library cannot prevent on
Viper's behalf — see [concurrency.md](concurrency.md).

## Filesystem caveats

Watching depends on the operating system's watcher, which does not see
changes written by another host. Network filesystems — NFS, SMB, FUSE — may
deliver no events at all. On one, reload from a signal or a timer instead.
The full policy is in [compatibility.md](compatibility.md).

## Configuration files

The library reads whatever it is pointed at, and does not police the path.
Two deployment rules are worth stating anyway:

- The file should be readable by the service user and writable by nobody
  else. A configuration file a local user can rewrite is a configuration a
  local user controls, within whatever the validator permits.
- The directory must be trusted too, because the watcher watches the
  directory and a file can be replaced by replacing what its name points at.

Neither is enforced here. A library that started refusing to read files
based on their mode would be wrong in someone's deployment on its first day.
