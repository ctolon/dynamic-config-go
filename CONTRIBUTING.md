# Contributing

## Getting set up

```bash
git clone https://github.com/ctolon/dynamic-config-go
cd dynamic-config-go

make test
```

Go 1.24 or later, and nothing else. `staticcheck` and `govulncheck` are
fetched by `go run ... @latest` when `make check` needs them, so there is no
tool installation step and no tool version to drift.

## Before opening a pull request

```bash
make check
```

That runs, in order: `gofmt`, `go vet`, `staticcheck`, the tests under the
race detector, a fuzz smoke run, and `govulncheck`. CI runs the same thing
across Linux, macOS and Windows, so a green `make check` locally is a good
predictor.

## What the project is trying to be

Small. The scope is deliberately narrow, and a feature that does not improve
the typed snapshot and hot-reload lifecycle is probably out of scope even if
it is a good idea:

- Viper owns configuration; this package owns safe publication.
- Every public method is a promise, and configuration promises are
  load-bearing for everything above them.
- Dependencies need justification. There are two, both unavoidable.

Remote stores, secret management, a plugin system, a configuration server, a
metrics endpoint, a templating engine, an expression language, a polling
fallback and a `WaitForGeneration` helper have all been considered and
declined, each for a reason worth reading before re-proposing it. See "What
is deliberately absent" in [docs/design.md](docs/design.md).

If you are unsure whether a change fits, open an issue before writing it.

## Changes that need tests

A change to any of these needs a test that would fail without it:

- the reload transaction, or the order of its stages;
- last-known-good behaviour;
- anything touching concurrency, publication or the lifecycle;
- watcher behaviour on any filesystem event;
- anything that could put a configuration value into a log or an error;
- any exported signature — `api_test.go` has to be updated deliberately.

The invariants in [docs/concurrency.md](docs/concurrency.md) are the list to
check a change against, and the compatibility policy in
[docs/compatibility.md](docs/compatibility.md) is what a change to the
public surface has to answer to.

## Style

The code follows the shape it already has: `gofmt`, standard Go naming, and
comments that explain why rather than what. A comment that restates the code
is noise; a comment recording the reason a decision was made — the one a
future reader would otherwise have to reconstruct — is the point.

Errors wrap with `%w` and name a stage:

```go
return fmt.Errorf("dynamicconfig: decode configuration: %w", err)
```

Never put a configuration value in an error message.

## Tests

Five layers, all run in CI:

| Layer | Where | What it covers |
| --- | --- | --- |
| unit | `config_test.go`, `lifecycle_test.go` | construction, options, reload, subscriptions, close/publish ordering |
| white-box | `internal_test.go` | handler retention, which is not observable from outside |
| surface | `api_test.go` | the exported signatures the compatibility policy freezes |
| integration | `integration/` | real files: writes, renames, deletion, permissions, projected volumes |
| concurrency | `integration/`, `lifecycle_test.go` | readers racing reloads, close racing publication, under `-race` |
| fuzz | `fuzz_test.go` | arbitrary documents, and random operation orderings checked against every invariant |
| benchmark | `benchmarks/` | `Current()` cost and its zero-allocation promise |

Prefer a deterministic test to a timing-dependent one: `WithDebounce(0)` and
an explicit `Reload` exercise the same transaction the watcher does. When a
test must wait for the filesystem, poll for the condition rather than
sleeping for a guess.

For orderings, force them. A validator that blocks on a channel stops a
reload at a known point — past read and decode, holding a candidate it has
not published — so a competing `Close` can be run to completion and the
winner asserted rather than hoped for. `lifecycle_test.go` has the pattern.

An assertion that depends on how fast the machine is will eventually fail on
somebody else's machine. Derive the bound from something measured (how long
a burst actually took) rather than assuming (that twenty-five writes fit in
one debounce window).

## Commits and pull requests

One logical change per pull request, with a description saying what the
change does and why. Update `CHANGELOG.md` under *Unreleased* for anything
user-visible.

## Reporting bugs

Include the Go version, the operating system, whether it happens under
Kubernetes, and — if reload is involved — how the file is being written
(direct write, rename, ConfigMap update). That last one is usually the
answer.

Security issues go through the process in [SECURITY.md](SECURITY.md), not
the issue tracker.

## Releasing

Maintainers only, and it is one script: see [RELEASING.md](RELEASING.md).
Contributors only need to leave an entry under `## [Unreleased]` in
`CHANGELOG.md`; the release rotates it.
