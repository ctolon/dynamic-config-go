# Security

## What this package is responsible for

A configuration library sits somewhere awkward. It reads files a process
trusts, and it holds values the process must not leak — a database password,
an API token, a signing key are all ordinary contents of a configuration
struct. These are the properties it tries to keep, and the ones it
explicitly does not.

## It tries to keep

**Configuration values stay out of diagnostics.** Nothing in this package
logs a configuration value. Errors name a stage, a path and a type:

```text
dynamicconfig: reload: validate configuration: server.port 70000 out of range
```

not

```text
database.password=hunter2
```

Log lines carry generations, counters, stages and file paths. `Change` has
no `String` or `MarshalJSON` method, so a change event cannot be rendered
into a log line by accident. `Status` contains counters, timestamps and
state only, which is what makes it safe to expose from an HTTP endpoint.

A configuration value appearing in a log line or an error message produced
by this package is a vulnerability. Please report it.

**No history is retained.** The package keeps the current snapshot and,
briefly, the previous one inside a queued change event. There is no
snapshot history, no ring buffer of past configurations, and no diagnostic
dump of decoded values.

**Queues are bounded.** Every internal queue has a fixed depth, so no rate
of filesystem events or reload failures can grow memory without limit. A
subscriber that stops consuming costs dropped events, counted in
`Status().DroppedEvents`, and nothing else.

**Failure never escalates.** A missing file, an unreadable file, a
half-written file, a file full of random bytes: all are rejected, reported
and survived. The process keeps running on the configuration it already had.
No panic, no exit, no silent demotion to defaults.

**The parsing boundary is fuzzed.** `FuzzReloadDocument` feeds arbitrary
documents through read, decode and validate, asserting that no input panics
and that no rejected input can disturb the published snapshot.
`FuzzSubscriberOperations` drives random sequences of subscribe,
unsubscribe, reload and close, looking for panics and deadlocks. Both run in
CI.

**Errors keep their chains.** Everything wraps with `%w`, so `errors.Is` and
`errors.As` work all the way down, and no error is reconstructed from a
string.

## It explicitly does not

**It does not redact.** The package cannot tell a password field from a port
number: `T` is the application's type, and Go offers no reliable signal for
which fields are secret. The rule is therefore blunt — it logs no values at
all — and the application remains responsible for what it does with the
snapshot it is handed:

```go
slog.Info("config", "value", cfg.Current())  // prints the password
```

Give secret fields a `String()` method that redacts, or log the fields you
mean to log.

**It does not encrypt, rotate or manage secrets.** It reads a file. Secret
management belongs to Vault, a cloud provider's secret store, or the
Kubernetes Secret machinery.

**It does not make Viper concurrency-safe.** Viper has no internal locking.
Reading `cfg.Viper` from another goroutine while a reload writes it is a
data race this package cannot prevent — only decline to pretend otherwise.
See [concurrency.md](concurrency.md).

**It does not validate the file's provenance.** Whoever can write the
configuration file can change the process's configuration, within whatever
the validator permits. File permissions and mount configuration are the
control here; the validator is a second, useful one.

**It does not fingerprint configurations.** Hashing a generic `T` means
serialising it, and serialising it means walking fields that may be secrets.
Deliberately absent.

## Threat model in one line

An attacker who can write the configuration file can reconfigure the process
within the validator's limits. An attacker who cannot write it can, at
worst, cause reload failures — and those leave the running configuration
untouched.

## Supply chain

- Two direct dependencies: Viper, and fsnotify, which Viper already brings.
  Nothing added for errors, logging, retries, worker pools, validation or
  metrics.
- CI runs `govulncheck ./...` on every push, and on a schedule.
- CI runs `go vet` and `staticcheck`.
- Every test runs under the race detector.
- Dependency updates arrive through automated pull requests and are
  reviewed.

## Reporting a vulnerability

Please report privately, through
[GitHub's advisory form](https://github.com/ctolon/dynamic-config-go/security/advisories/new),
rather than in a public issue.

Include what you would want if you were on the other side: what an attacker
can do, what they need to start with, and — if you have one — a
reproduction. A first response should take a few days; if it takes longer
than a week, assume the message went astray and ping the issue tracker
without details.
