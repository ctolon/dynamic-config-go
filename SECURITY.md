# Security

## Reporting a vulnerability

Please report privately, through
[GitHub's advisory form](https://github.com/ctolon/dynamic-config-go/security/advisories/new),
rather than in a public issue.

Include what you would want if you were on the other side: what an attacker
can do, what they need to start with, and — if you have one — a
reproduction. A first response should take a few days; if it takes longer
than a week, assume the message went astray and ping the issue tracker
without details.

## Supported versions

The most recent release. This project is pre-1.0; fixes land on `main` and
in the next release rather than being backported.

## What this library promises

The full account is in [docs/security.md](docs/security.md). In short:

- No configuration value is ever logged by this package, and no error
  message contains one.
- `Status` carries counters, timestamps and state only, so it is safe to
  expose from a health endpoint.
- No configuration history is retained, and every internal queue is bounded.
- A missing, unreadable, half-written or malformed file is rejected and
  survived — never a panic, never an exit, never a silent demotion to
  defaults.
- The read/decode/validate boundary and the subscription machinery are
  fuzzed in CI.

And what it does not promise: it does not redact fields (it cannot tell a
password from a port), it does not manage or encrypt secrets, and it cannot
make concurrent use of `cfg.Viper` safe, because Viper does no locking of
its own.

A configuration value appearing in a log line or error message produced by
this package is a vulnerability. Please report it.
