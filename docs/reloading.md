# Reloading

## The transaction

A reload behaves like a transaction whose commit cannot fail:

```text
BEGIN

1. take the reload slot          (serialisation)
2. read the file into Viper      (StageRead)
3. decode into T                 (StageDecode)
4. validate T                    (StageValidation)
5. take the commit gate
6. refuse if closing             ← the only reason a valid candidate is dropped
7. publish atomically, advance the generation, record the file stamp
8. release the commit gate
9. release the reload slot
10. enqueue the change event

COMMIT
```

Steps 2 to 4 are the fallible ones and they all happen before step 7, so
there is no rollback to write. A failure anywhere leaves the published
snapshot exactly as it was — that is the whole of the last-known-good
guarantee.

Steps 5 to 8 are the commit, and they hold a lock shared with exactly one
other thing: the transition to closing. That gives publication and shutdown
a total order — one of them is first — so a reload can never publish a
generation after `Close` has returned. See
[concurrency.md](concurrency.md#the-commit-gate). Nothing expensive happens
under that lock: no file, no decoder, no validator, no callback.

The manual path and the watcher path run the same transaction. There is no
second implementation to drift apart from the first.

## Layered files

A reload reads every configured file, in the order the options named them:

```text
config.yaml         read      ← replaces whatever Viper held
secret.yaml         merged    ← overrides keys config.yaml set
config.local.yaml   merged    ← overrides both, and may be absent
       │
       ▼
   decode into T
       │
   validate once
       │
   publish once
```

The first file present is *read* rather than merged, which is what stops a
key deleted from a file surviving into the next snapshot. Everything after
it merges over the top, so a later file wins a conflict and a key nobody
overrides keeps the value the base file gave it.

The whole set is one transaction. One decode, one validator run — so a rule
spanning two files has somewhere to live — and one generation, however many
files it came from. A file that is missing, unreadable or unparseable
rejects the *candidate*, not just its own layer: publishing the rest would
mean quietly demoting a service to its base configuration the moment a
secret file went missing.

`WithOptionalConfigFile` is how a file is allowed to be absent. Absent, and
nothing else — a file that exists and cannot be parsed still fails.

## Triggering a reload

**Automatically**, from a file change:

```go
go cfg.Watch(ctx)
```

**Manually**, from an admin endpoint, a test, or a signal:

```go
if err := cfg.Reload(ctx); err != nil {
    // The candidate was rejected. Current() is unchanged.
}
```

SIGHUP needs no support from this package:

```go
hangups := make(chan os.Signal, 1)

signal.Notify(hangups, syscall.SIGHUP)

go func() {
    for range hangups {
        if err := cfg.Reload(ctx); err != nil {
            slog.Error("manual reload failed", "error", err)
        }
    }
}()
```

## Debouncing

One save is not one event. An editor typically produces something like:

```text
WRITE
WRITE
CHMOD
RENAME
CREATE
```

Reloading five times would publish five generations for one logical change.
Events are therefore collapsed: a reload runs once the events have been
quiet for the debounce window.

```go
dynamicconfig.WithDebounce[AppConfig](200 * time.Millisecond)
```

The default is 200 ms — long enough to fold a save, short enough that a
deliberate change appears to take effect immediately. Zero disables the
window, which is useful in tests. The value is a tuning knob, not part of
the contract.

The window is a *quiet* window: every event restarts it. A file rewritten
continuously, faster than the window, therefore never goes quiet and never
reloads until the writes pause. That is the intended behaviour of a
debounce, and the reason zero exists.

## Coalescing

Debouncing alone does not bound an event storm — events can keep arriving
while a reload is running. The state machine does:

```text
IDLE ──event──► PENDING ──window elapses──► RELOADING ──► IDLE
                   ▲                            │
                   └────────── event ───────────┘
```

At most one further reload is ever pending. A thousand events during a
reload produce exactly one follow-up, so memory stays flat whatever the
filesystem does.

## What a reload failure means

It means a candidate was rejected. It never means the configuration was
lost.

| Stage | Cause | Effect |
| --- | --- | --- |
| `StageRead` | file missing, unreadable, unparseable | rejected; previous snapshot stays |
| `StageDecode` | shape does not fit `T` | rejected; previous snapshot stays |
| `StageValidation` | the validator said no, or panicked | rejected; previous snapshot stays |
| `StageWatch` | the filesystem watcher itself erred | reported; a reload is attempted |
| `StageCallback` | a subscriber panicked | the reload already succeeded |

Failures increment `Status().FailedReloads` and are delivered to error
subscribers. There is no automatic retry: a file that does not validate now
will not validate in a second, and the next filesystem event is a better
retry than a timer.

One error is not in that table. A reload that reaches its commit after
`Close` has won returns `ErrClosed`: it is neither counted as a failure nor
reported to subscribers, because a shutdown is not a verdict on the
configuration.

## The validator

The validator decides what may be published, and it is ordinary Go:

```go
func validate(c *AppConfig) error {
    if c.Database.MaxIdle > c.Database.MaxOpen {
        return fmt.Errorf("database.max_idle %d exceeds max_open %d",
            c.Database.MaxIdle, c.Database.MaxOpen)
    }

    return nil
}
```

Its execution contract:

- It runs **synchronously**, inside the transaction, on the goroutine that
  called `Reload` or on the watcher's.
- It should be **quick, local and deterministic**. Network calls make reload
  latency unbounded and tie whether a configuration is acceptable to whether
  a dependency is up.
- It must be **safe to call from any goroutine** if it touches shared state,
  because which goroutine runs it depends on what triggered the reload.
- It must not call `Reload` or `Close`: both wait on the transaction it is
  running inside.
- It may allocate, and it should wrap its errors.
- Its errors are **logged when a logger is configured**, so they must not
  contain configuration values.

A **panic in a validator is recovered** and turned into a rejected
candidate. A validator is application code on a recoverable path, and a
mistake in a rule about port ranges should cost that candidate, not the
process. During construction the same panic becomes an error from the
constructor.

## Events

```go
sub := cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
    slog.Info("configuration reloaded",
        "generation", change.Generation,
        "source", string(change.Source),
    )
})

defer sub.Unsubscribe()
```

A `Change` carries the previous snapshot (nil for the initial load), the
current one, the generation, the time and the source (`initial`,
`filesystem` or `manual`).

`Change` has no `String` or `MarshalJSON` method on purpose. Rendering one
would render configuration values, and a configuration struct is exactly the
kind of thing that grows a password field later. Log the generation.

### The delivery contract

> Publication is reliable. Delivery to subscribers is best-effort and
> bounded. A subscriber that needs authoritative state must read `Current()`.

Handlers run on one dispatcher goroutine, one at a time, in publication
order — ordering is worth more than parallelism for configuration, and a
subscriber that sees generation 12 before generation 11 has been actively
misled.

Between publication and delivery is a bounded queue (`WithEventBuffer`,
default 16). If a subscriber is too slow to keep up, the oldest pending
notification is dropped and `Status().DroppedEvents` advances. Dropping the
oldest is the right choice here: for configuration, the newest state is the
one that matters.

What this buys is the property that matters more:

```go
cfg.Subscribe(func(dynamicconfig.Change[AppConfig]) {
    time.Sleep(time.Minute)  // a subscriber having a bad day
})
```

`Current()` already returns the new configuration. Reloads keep succeeding.
Nothing waits for this handler.

### Panics

```go
cfg.Subscribe(func(dynamicconfig.Change[AppConfig]) {
    panic("oops")
})
```

The panic is recovered, reported to the error subscribers as a
`StageCallback` failure, and costs that one callback. Other subscribers
still receive the event, the dispatcher survives, and the watcher never
learns of it.

A panic in an *error* handler is logged but not reported as another error
event — otherwise a handler that panics on every error would feed itself.

### Delivery and shutdown

`Close` stops the dispatcher after freezing publication. No callback starts
once the worker has observed the stop; one already running gets up to five
seconds to return; queued notifications are abandoned. Shutdown has to be
bounded even when a subscriber is not.

## Generations

Every successful publication increments the generation:

```text
startup        generation 1
reload         generation 2
reload fails   generation 2   ← unchanged
reload         generation 3
```

`Generation()` is the cheapest way to confirm that a reload actually took
effect, and a generation that stops moving while `FailedReloads` climbs is
the signature of a configuration file that no longer validates.

`Generation == SuccessfulReloads + 1` always holds: the initial load
publishes generation 1 without counting as a reload.

Identical content still produces a new generation. Comparing decoded values
would mean `reflect.DeepEqual` over an arbitrary `T`, which is both
expensive and quietly wrong for some types, so the contract is the
predictable one instead. The single exception is the check `Watch` makes at
startup, which compares the file's size, mode and modification time — not
its content — so that starting a watcher over an untouched file does not
manufacture a generation.

## What does not trigger a reload

Only files produce events. In particular:

- **Environment variables.** Viper reads them when asked, so a variable that
  changes inside the running process is picked up by the next reload — but
  nothing schedules that reload. Call `Reload(ctx)`.
- **`cfg.Viper().Set(...)`.** It changes Viper's state, not the published
  snapshot. Call `Reload(ctx)`.
- **Defaults and aliases registered after construction.** Same rule.
- **A file on a network filesystem.** Inotify and kqueue watch a local
  kernel's view; a change written by another host may produce no local event
  at all. See [compatibility.md](compatibility.md#filesystems).
