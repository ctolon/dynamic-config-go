# Reloading

## The transaction

A reload behaves like a transaction whose commit cannot fail:

```text
BEGIN

1. take the reload slot        (serialisation)
2. read the file into Viper    (StageRead)
3. decode into T               (StageDecode)
4. validate T                  (StageValidation)
5. publish atomically          ← the only step that cannot fail
6. increment the generation
7. release the slot
8. enqueue the change event

COMMIT
```

Steps 2 to 4 are the fallible ones and they all happen before step 5, so
there is no rollback to write. A failure anywhere leaves the published
snapshot exactly as it was — that is the whole of the last-known-good
guarantee.

The manual path and the watcher path run the same transaction. There is no
second implementation to drift apart from the first.

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
| `StageValidation` | the validator said no | rejected; previous snapshot stays |
| `StageWatch` | the filesystem watcher itself erred | reported; a reload is attempted |
| `StageCallback` | a subscriber panicked | the reload already succeeded |

Failures increment `Status().FailedReloads` and are delivered to error
subscribers. There is no automatic retry: a file that does not validate now
will not validate in a second, and the next filesystem event is a better
retry than a timer.

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
