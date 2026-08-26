# Releasing

A Go module is released by pushing a tag. There is no registry to publish
to, no artefact to upload and no version file to edit — `proxy.golang.org`
fetches the tag, and `pkg.go.dev` documents it.

That makes the tag the only thing that has to be trustworthy, and everything
below exists to keep it that way.

## The branch model

Work lands on `dev` through pull requests, with entries accumulating under
`## [Unreleased]` in `CHANGELOG.md`. `main` is what has been released, or is
about to be.

**The tag is the release.** `release.yml` runs on a pushed `v*` tag: it
re-runs the whole gate against the exact commit the tag names, refuses a tag
that is not reachable from `main`, refuses a tag the changelog does not
describe, publishes the GitHub release from that changelog section, and asks
the module proxy to index the version.

Re-running CI at tag time rather than trusting an earlier green run is the
point. The commit that gets tagged is not always the commit that was tested.

## Cutting a release

```bash
git switch main && git pull

./scripts/release.sh patch     # or minor, major, or 0.4.0 outright
```

The script refuses a dirty tree, refuses a branch that is not `main`,
refuses a tag that already exists and refuses an empty `Unreleased`
section. Then it runs `make check` — gofmt, vet, staticcheck, the tests
under the race detector, a fuzz smoke run and `govulncheck` — rotates the
changelog, commits, and annotates a tag with the release notes.

It pushes nothing. Read what it made:

```bash
git show HEAD
git show v0.1.0
```

Then publish:

```bash
git push origin main
git push origin v0.1.0
```

Pushing the tag is the release. Watch it:

```bash
gh run watch
```

## What the workflow checks before publishing

| Check | Why |
| --- | --- |
| the tag is reachable from `main` | a release cut from a branch is a release nobody can find again |
| `CHANGELOG.md` has a section for the version | a release with no notes |
| `gofmt`, `go vet`, `staticcheck` | the ordinary gate, at the tagged commit |
| `go test -race ./...` | a concurrency library's release blocker |
| fuzz smoke, both targets | the parse and subscription boundaries |
| `TestCurrentDoesNotAllocate` | the headline performance promise |
| examples build | the documentation compiles |
| `govulncheck ./...` | not shipping a known-vulnerable dependency |

Only then does it create the GitHub release, and only then does it ask
`proxy.golang.org` for the module — which is what makes the version appear
on `pkg.go.dev`.

## Versioning

[Semantic versioning](https://semver.org/). Below 1.0 the API may still
move, and a minor version is where that happens.

From 1.0, the contracts listed in
[docs/design.md](docs/design.md#compatibility-policy) are stable and
breaking one requires a major version: `Config[T]`, `New`, `Wrap`,
`Current`, `Reload`, `Watch`, `Close`, the read/decode/validate/publish
ordering, last-known-good behaviour, subscription semantics and error
wrapping.

Adding a `Status` field or an option is a minor release. Changing what
`Current()` may return is not a thing that happens.

A tag with a hyphen — `v0.2.0-rc.1` — publishes as a pre-release and does
not become `latest`.

## Go's rules to keep in mind

- **Tags are immutable in practice.** The proxy caches a version the first
  time anyone fetches it. A tag that is moved after publication stays wrong
  for everyone who already has it. Cutting `v0.1.1` is the fix; retagging is
  not.
- **`v2` and beyond need a path change.** A major version above 1 lives at
  `github.com/ctolon/dynamic-config-go/v2`, in `go.mod` and in every import.
  That is Go's rule, not this project's.
- **The minimum Go version is a compatibility promise.** Raising `go 1.24`
  in `go.mod` is a minor-version change at least, and belongs in the
  changelog.
- **`retract`** is how a broken version is withdrawn. It goes in `go.mod`,
  in a *later* release — a published version cannot be unpublished.

## After a release

- `pkg.go.dev` usually shows the version within a few minutes. The
  workflow's last job nudges it; if it warns, the proxy simply has not
  caught up yet.
- Check the rendered documentation once. It is the first thing a new user
  reads, and package-level examples that do not compile show up there rather
  than in CI.
- `Unreleased` is empty again, and `dev` continues.
