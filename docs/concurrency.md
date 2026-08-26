# Concurrency

## The asymmetry

Configuration is read constantly and written rarely. A busy service may call
`Current()` a million times a minute and reload twice a week. Every design
decision here follows from that ratio.

```text
READS                        WRITES
─────                        ──────
Current()                    Reload()
   │                            │
atomic.Load                  acquire the reload slot
   │                            │
return                       read ─► decode ─► validate
                                │
                             atomic.Store
                                │
                             release
No lock.                     One at a time.
```

Readers never acquire a lock, never block, never contend with each other and
never contend with a reload. A reload does all of its expensive and fallible
work off to the side and becomes visible in a single atomic store.

## What is safe

Concurrently, from any number of goroutines, in any combination:

- `Current()`
- `Status()`, `Generation()`, `ReloadCount()`
- `Reload(ctx)` — attempts are serialised internally
- `Subscribe`, `SubscribeErrors`, `Unsubscribe`
- `Close()` — idempotent, and safe while a watcher is running
- reading a snapshot while a reload publishes a new one
- a subscriber callback calling back into the `Config`, including `Reload`

One watcher may run per `Config`. A second concurrent `Watch` returns
`ErrAlreadyWatching` rather than inventing semantics for two watchers
sharing one debounce window.

## What is not safe

**Mutating a snapshot.**

```go
cfg.Current().Server.Port = 8081       // unsupported
cfg.Current().Features["beta"] = true  // unsupported, and a data race
```

Every reader holding that snapshot sees the write, with no synchronisation
at all. Copy what you need to change.

**Using `cfg.Viper` from another goroutine while reloads can run.** Viper
does no internal locking, and a reload writes Viper's state. A concurrent
`cfg.Viper.GetString(...)` next to a reload is a data race that this package
cannot fix on Viper's behalf — it can only decline to pretend otherwise.

Configure Viper before the first reload (through options, `WithViperSetup`,
or before `Wrap`), and read the application's configuration through
`Current()`.

## The invariants

These are asserted in tests, and each has a reason to exist.

1. **After a successful construction, `Current()` is not nil.** Application
   code never has to consider a nil configuration.
2. **Only validated configurations are published.** Publication is the last
   step of the transaction, after everything that can fail.
3. **A failed reload never modifies the published snapshot.** There is no
   rollback because there is nothing to roll back.
4. **Readers never acquire the reload lock.** The read path is an atomic
   load.
5. **Only one reload transaction runs at a time.** Two concurrent stores
   would make event ordering meaningless.
6. **Callbacks run outside every lock.** A subscriber may call `Reload`.
7. **A callback panic cannot take down the watcher.** It costs that callback
   and nothing else.
8. **`Close()` is idempotent.** Calling it three times is not an error.
9. **Every internal queue is bounded.** No event rate can grow memory
   without limit.
10. **No configuration value is logged by this package.**

## How reloads are serialised

Not with a `sync.Mutex`. The reload slot is a one-element channel:

```go
select {
case c.reloadSem <- struct{}{}:
case <-ctx.Done():
    return fmt.Errorf("dynamicconfig: reload: %w", ctx.Err())
case <-c.life.Done():
    return ErrClosed
}
```

A mutex cannot be waited on with a deadline. A caller with a context — an
admin endpoint, a watcher being torn down — needs a way out of the wait, and
this gives it one.

The slot is released before anything touches user code, which is what makes
invariant 6 true.

## Snapshot lifetime

Old snapshots stay alive as long as someone holds them:

```text
snapshot 10 ─── a request that started before the reload
snapshot 11 ─── current
```

That is ordinary Go garbage collection, and it is what makes a long request
consistent: it keeps working on the configuration it started with. Snapshot
10 becomes collectable when the last reference goes away.

The package keeps no history of its own. The only place a previous snapshot
is retained is a `Change` in the dispatch queue, and that queue is bounded —
which is one more reason it has to be.

## Race detector

Every test runs under `-race` in CI, including a stress test with 64
concurrent readers checking for torn snapshots while reloads and rejections
alternate. A race-detector failure in a library that claims concurrency
safety is a release blocker, not a flake to be re-run.

```bash
go test -race ./...
```
