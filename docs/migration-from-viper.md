# Migrating from Viper

Nothing has to be rewritten. The migration is incremental, and the first
step is a single call.

## Before

```go
viper.SetConfigFile("config.yaml")

if err := viper.ReadInConfig(); err != nil {
    return err
}

var cfg AppConfig

if err := viper.Unmarshal(&cfg); err != nil {
    return err
}

// ... and everywhere else in the application:
port := viper.GetInt("server.port")
```

Two problems, both quiet. The configuration is never validated, so a typo
becomes a runtime surprise somewhere far from the file. And nothing reloads
— or, if `WatchConfig` was added, reloads race with every reader.

## After

```go
v := viper.New()

v.SetConfigFile("config.yaml")
v.SetDefault("server.port", 8080)
v.SetEnvPrefix("MYAPP")
v.AutomaticEnv()

cfg, err := dynamicconfig.Wrap[AppConfig](v,
    dynamicconfig.WithValidator(validateConfig),
)
if err != nil {
    return fmt.Errorf("initialize configuration: %w", err)
}

defer cfg.Close()

// ... and everywhere else in the application:
port := cfg.Current().Server.Port
```

Everything already configured on that Viper instance keeps working:
defaults, environment binding, aliases, search paths, formats. `Wrap` adopts
the instance rather than replacing it, and `cfg.Viper()` returns the same pointer
that was passed in.

## Step by step

### 1. Wrap the instance

Keep the Viper setup exactly as it is; add `Wrap`. Nothing else has to
change yet — `cfg.Viper().GetInt("server.port")` still works, so existing call
sites keep compiling.

### 2. Add a validator

This is where most of the value is, and it is ordinary Go:

```go
func validateConfig(c *AppConfig) error {
    if c.Server.Port < 1 || c.Server.Port > 65535 {
        return fmt.Errorf("server.port %d is outside 1-65535", c.Server.Port)
    }

    if c.Database.MaxIdle > c.Database.MaxOpen {
        return fmt.Errorf("database.max_idle %d exceeds max_open %d",
            c.Database.MaxIdle, c.Database.MaxOpen)
    }

    return nil
}
```

From now on the process refuses to start on a configuration it does not
understand, and refuses to *adopt* one at runtime.

### 3. Move reads to `Current()`

Replace `viper.GetX("some.key")` with a field read on a snapshot, one call
site at a time. Read one snapshot per unit of work:

```go
func handle(w http.ResponseWriter, r *http.Request) {
    current := cfg.Current()

    use(current.Server.Host, current.Server.Port)
}
```

Not:

```go
use(cfg.Current().Server.Host, cfg.Current().Server.Port)  // may straddle a reload
```

The typed struct also means a mistyped key is now a compile error rather
than a zero value at three in the morning.

### 4. Turn on watching

```go
go func() {
    if err := cfg.Watch(ctx); err != nil && !errors.Is(err, context.Canceled) {
        slog.Error("configuration watcher stopped", "error", err)
    }
}()
```

### 5. Drop the globals

A `*Config[T]` passed to constructors is easier to test than a package-level
`viper` singleton, and a test can build one over a `t.TempDir()` file in
three lines.

## Replacing viper.WatchConfig

If the application already used Viper's own watcher, remove it. This package
runs its own, for reasons that are worth knowing:

- Viper's watcher stops permanently when the configuration file is removed.
  Deleting and recreating a file — which is what `mv` over it, some editors,
  and some deployment tools do — silently ends hot reload for the lifetime
  of the process.
- It cannot be stopped, so it outlives whatever created it.
- A read failure inside it is invisible to the application; `OnConfigChange`
  is only called on success, so there is no way to observe a broken
  configuration file.
- It has no debouncing, so one save can fire several callbacks.

The watcher here handles deletion and re-creation, stops with its context or
with `Close`, reports read failures through `SubscribeErrors`, debounces
bursts, and understands Kubernetes projected volumes.

## Things to keep in mind

**`cfg.Viper().Set` does not publish.** It changes Viper's state. A reload
publishes:

```go
cfg.Viper().Set("feature.enabled", true)

if err := cfg.Reload(ctx); err != nil {
    return err
}
```

**Do not read `cfg.Viper()` from other goroutines once reloads can run.**
Viper does no locking, and a reload writes its state. When the migration is
finished and nothing needs raw key access any more, `WrapSealed` closes that
route for good.

**Snapshots are immutable by contract.** Do not write through the pointer
`Current()` returns, or through the maps and slices inside it.

**Keep using Viper for what Viper does.** Defaults, environment variables,
aliases, formats and search paths are still Viper's, through `cfg.Viper()`
or `WithViperSetup`.

**Environment changes do not reload themselves.** Only files produce events.
A variable that changes inside the running process is picked up by the next
`Reload(ctx)`, and nothing schedules that call for you.
