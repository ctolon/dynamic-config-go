# Troubleshooting

## The file changed and nothing reloaded

Work down this list; it is ordered by how often each turns out to be the
answer.

**Is the watcher running?**

```go
cfg.Status().Watching  // false means nothing is watching
```

`Watch` blocks, so it belongs on its own goroutine — and a `Watch` that
returned early because of an error is the usual reason this is false:

```go
go func() {
    if err := cfg.Watch(ctx); err != nil {
        slog.Error("watcher stopped", "error", err)  // do not discard this
    }
}()
```

**Is the reload being rejected?** A rejected reload is not a silent one, but
it is silent if nobody subscribes:

```go
cfg.SubscribeErrors(func(e dynamicconfig.ReloadError) {
    slog.Error("reload rejected", "stage", e.Stage, "error", e.Err)
})
```

`Status().FailedReloads` climbing while `Generation` stays flat is the same
signal, in counter form.

**Is it a Kubernetes `subPath` mount?** A `subPath` mount never updates. See
[kubernetes.md](kubernetes.md).

**Is the application reading the engine instead of the snapshot?**
`cfg.Viper().GetInt(...)` reads Viper's state, which is a different thing
from the published snapshot — and is a data race next to a reload. Sealing
the configuration (`NewSealed`, `WrapSealed`) makes this mistake
impossible.

**Did anything actually change on disk?** Only files produce events. A
variable in the process environment, a `cfg.Viper().Set(...)`, or a default
registered after construction all wait for the next `Reload(ctx)`, and
nothing schedules that call for you.

**Is the value cached somewhere?** A snapshot read once at startup and
stored in a struct field will never change. Read `Current()` per unit of
work.

**Is the file a symlink to another directory?** The watcher watches the
directory containing the configured path. A symlink pointing somewhere else
entirely means changes happen in a directory nobody is watching. Point the
configuration at the real path, or at a Kubernetes-style `..data` layout,
which is handled. On macOS, kqueue follows a watched symlink to its target,
so replacing the link itself produces no event at all.

**Is the file on a network filesystem?** NFS, SMB and FUSE often deliver no
events: the local kernel does not see a write made by another host. This is
not slowness, it is silence, and no library can invent the event. Reload
from a signal or a timer instead — see
[compatibility.md](compatibility.md#not-guaranteed).

## A file being rewritten constantly never reloads

The debounce window is a *quiet* window: each event restarts it. A file
rewritten faster than the window — a templating sidecar in a tight loop, a
test writing every millisecond — therefore never goes quiet, and the reload
never fires until the writes pause.

This is the intended behaviour, and it is worth knowing rather than
debugging. If the writer is pathological, fix the writer; if the workload
genuinely produces continuous changes, lower or disable the window:

```go
dynamicconfig.WithDebounce[AppConfig](0)   // reload per event, still coalesced
```

Zero does not mean unbounded work: a reload still runs alone, and events
that arrive while one is running still collapse into at most one more.

## It reloads several times for one save

Raise the debounce window:

```go
dynamicconfig.WithDebounce[AppConfig](500 * time.Millisecond)
```

Some editors write, truncate, rename and chmod over a longer interval than
the 200 ms default covers.

## New() fails at startup but the file looks fine

The error names the stage. Read it in full — it wraps its cause.

- *read* — the path is wrong, the file is unreadable, or the format cannot
  be parsed. An extensionless file needs an explicit type:
  `v.SetConfigType("yaml")` inside `WithViperSetup`.
- *decode* — the document does not fit `T`. A missing `mapstructure` tag is
  the usual cause; the field name and the key must match, or the tag must
  bridge them.
- *validation* — the validator said no, and its message is included.

An environment-only configuration with no file at all needs
`WithAllowMissingFile(true)`, or it fails fast by design.

## Current() returns zero values

The document parsed but did not decode into the fields expected. Check the
`mapstructure` tags:

```go
type ServerConfig struct {
    Host string `mapstructure:"host"`  // not `yaml:"host"`
    Port int    `mapstructure:"port"`
}
```

Viper decodes through mapstructure, so `yaml`, `json` and `toml` tags are
ignored. A validator that requires the fields it needs turns this into a
startup error rather than a silent zero.

## Durations or custom types do not decode

Add the decode hook Viper needs:

```go
dynamicconfig.WithDecodeOption[AppConfig](
    viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
        mapstructure.StringToTimeDurationHookFunc(),
        mapstructure.StringToSliceHookFunc(","),
    )),
)
```

The option applies to every reload, so the decode at reload matches the
decode at startup.

## A subscriber is not being called

- Was `Unsubscribe` called, perhaps by a `defer` in a function that already
  returned?
- Is another subscriber blocking the dispatcher? Handlers run one at a time,
  in order, so one handler that never returns stops the rest. Check
  `Status().DroppedEvents`.
- Is the `Config` closed? A closed configuration delivers nothing.

## DroppedEvents keeps rising

A subscriber cannot keep up. Handlers run on one goroutine, so slow work
belongs elsewhere:

```go
cfg.Subscribe(func(change dynamicconfig.Change[AppConfig]) {
    select {
    case work <- change.Generation:  // hand it off
    default:
    }
})
```

Raising `WithEventBuffer` buys headroom but does not make delivery reliable.
Anything that needs authoritative state should read `Current()`.

## Viper() returns nil

The configuration is sealed — built with `NewSealed` or `WrapSealed` — so
the engine is deliberately unreachable through it. Configure the engine at
construction, with the options or `WithViperSetup`, and read the
application's configuration through `Current()`. `cfg.Sealed()` is the
explicit form of the check.

If a subsystem genuinely needs raw key access, build the configuration with
`New` or `Wrap` instead, and treat `cfg.Viper()` as construction-time state.

## Reload returns ErrClosed

The configuration was closed, or a `Close` won the race to the commit while
this reload was still reading, decoding or validating. The candidate was not
published, nothing was disturbed, and it is not counted as a failed reload:
a shutdown is not a verdict on the configuration.

## Close() returns an error

A subscriber callback was still running when the shutdown wait expired. The
configuration is closed either way; the message means some handler is
blocking for longer than five seconds. Find it — it is also delaying every
other subscriber's events.

## Watch returns ErrAlreadyWatching

Only one watcher may run per `Config`. Two goroutines called `Watch`, or a
previous watcher has not finished stopping. Wait for the first to return
before starting another.

## Tests are flaky around reloads

Do not sleep and hope; poll:

```go
waitFor(t, func() bool { return cfg.Generation() > before })
```

Better, skip the filesystem entirely — `WithDebounce(0)` and an explicit
`cfg.Reload(ctx)` make the test deterministic, and it is the same
transaction the watcher runs.
