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
- `Status()`, `Generation()`, `ReloadCount()`, `Sealed()`
- `Reload(ctx)` — attempts are serialised internally
- `Subscribe`, `SubscribeErrors`, `Unsubscribe`
- `Close()` — idempotent, and safe while a watcher or a reload is running
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

**Using an open `Config`'s engine while reloads can run.** Viper does no
internal locking, and a reload writes Viper's state. A concurrent
`cfg.Viper().GetString(...)` next to a reload is a data race that this
package cannot fix on Viper's behalf — it can only decline to pretend
otherwise.

```go
// Startup, before anything can reload:
cfg.Viper().SetDefault("server.port", 8080)

// Runtime:
current := cfg.Current()          // yes
port := cfg.Viper().GetInt(...)   // no
```

Sealing turns that rule from advice into structure: `NewSealed` and
`WrapSealed` produce a `Config` whose `Viper()` returns nil, so there is
nothing to race and nothing to spread through a codebase. Everything else
behaves identically.

**A validator that calls back into its own `Config`.** `Reload` and `Close`
both wait on the transaction the validator is running inside.

## The commit gate

Two events must never interleave: the commit of a new snapshot, and the
transition to closing. Without an order between them, this is possible:

```text
G1 Reload   checks "not closed"      → false
G2 Close    transitions to closing
G2 Close    stops the dispatcher, returns
G1 Reload   publishes generation 12          ← after Close returned
```

An application that shut its configuration down would then see it change
underneath the work it was finishing.

A one-line mutex — `publishMu` — gives the two a total order. `Close` makes
its lifecycle transition under it, and a reload makes its commit under it:

```text
publication commit   XOR   close transition
```

One of them is first. If the commit is first, the generation is published
and then frozen; if the close is first, the reload finds the `Config` closed
at its commit and returns `ErrClosed` without publishing.

That error is *not* counted as a failed reload, and not delivered to error
subscribers. A shutdown is not a verdict on the configuration.

The critical section is deliberately tiny. Nothing is read from disk,
nothing is decoded, no validator runs and no callback fires while it is
held — it guards a handful of atomic stores and a lifecycle transition, and
nothing else. Reads never touch it, so `Current()` is unaffected.

## The invariants

Each of these is a test, and each has a reason to exist.

**Publication**

1. Successful construction publishes exactly generation 1.
2. `Current()` is non-nil after successful construction.
3. Only candidates that were read, decoded and validated are published.
4. A failed reload never changes `Current()`.
5. Generation advances exactly once per publication.
6. Generation never decreases.
7. `Generation == SuccessfulReloads + 1`, always.

**Concurrency**

8. `Current()` performs no allocation and takes no lock.
9. Readers never acquire the reload semaphore or the commit gate.
10. Reload transactions are serialised — never two at once.
11. Callbacks run outside every lock, so a handler may call `Reload`.
12. A callback panic cannot crash the reload or watch machinery.
13. A validator panic becomes a rejected candidate, not a dead process.

**Lifecycle**

14. Once close begins, no new snapshot is ever published.
15. `Current()` stays readable, and frozen, after `Close()`.
16. `Reload` after `Close` returns `ErrClosed`.
17. `Watch` after `Close` returns `ErrClosed`.
18. `Close` is idempotent.
19. At most one watcher owns a `Config`.
20. The watcher's lifetime is owned by the application.
21. A subscription made after close retains no handler.
22. `Unsubscribe` is idempotent and never panics.

**Bounds and delivery**

23. Every internal queue is bounded.
24. Delivered events arrive in publication order.
25. Delivery is best-effort; `Current()` is authoritative.

**Behaviour**

26. A configuration file that disappears never silently falls back to
    defaults.
27. The gap between loading and watching is reconciled at watcher startup.
28. Kubernetes projected-volume swaps are detected on Linux.
29. No configuration value is logged by the library.

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

## Shutdown ordering

`Close` does four things, in this order, and the order is the point:

```text
freeze publication      ← the commit gate; nothing is published after this
      │
stop the watcher        ← whether or not its context was cancelled
      │
stop the dispatcher     ← waits for a running callback, abandons queued work
      │
drop subscriptions      ← retaining no handler
```

Freezing first is what makes the rest safe to describe: after step 1 there
is no generation left to be produced, so steps 2 to 4 are cleanup rather
than a race.

The dispatcher's contract is the honest one rather than the tidy one: no
task starts after the worker has observed the stop, but a task already
dequeued may finish. Promising instantaneous cancellation would need a gate
on every dequeue to buy a guarantee nobody can observe. The worker checks
for shutdown before it looks at the queue at all, so a full queue cannot
keep starting callbacks after `Close`.

Subscriptions are dropped inside the registry's own lock, not by checking
the lifecycle first. A check-then-add would let a subscription arriving
alongside `Close` survive it, keeping whatever the handler captured — a
pool, a cache, a whole service — reachable for as long as anything held the
`Config`.

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

## How the invariants are tested

Not with sleeps. The orderings that matter are forced.

**A validator barrier** stops a reload at a known point — past read and
decode, holding a valid candidate it has not published — while a competing
`Close` runs to completion. Releasing the barrier then proves which of the
two won, deterministically, and the test runs a hundred times under `-race`:

```go
func (b *barrier) validator(_ *appConfig) error {
    if b.armed.CompareAndSwap(true, false) {
        close(b.entered)

        <-b.release
    }

    return nil
}
```

**A model fuzzer** drives random sequences of subscribe, unsubscribe,
reload, write, close and read, checking the invariants above after every
single operation. Fuzzing a parser looks for inputs that crash it; this
looks for *orderings* that break a promise, which is where a concurrent
lifecycle actually goes wrong.

**White-box registry tests** assert that a closed configuration retains no
handler. That is a memory-lifetime property — a retained handler that never
runs looks exactly like no handler at all from outside — so it is checked
from inside the package.

**A stress test** runs 64 concurrent readers checking for torn snapshots
while reloads and rejections alternate.

**Goroutine accounting** walks every lifecycle that starts a goroutine —
watch cancelled, watch closed without cancellation, subscriber panic, reload
storm — and waits for the count to come back.

## Race detector

Every test runs under `-race` in CI, on Linux, macOS and Windows. A
race-detector failure in a library that claims concurrency safety is a
release blocker, not a flake to be re-run.

```bash
go test -race ./...
```
