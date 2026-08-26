## What this changes

<!-- And why. If it fixes an issue, link it. -->

## Checklist

- [ ] `make check` passes (gofmt, vet, staticcheck, `-race` tests, fuzz smoke, govulncheck)
- [ ] Tests that would fail without this change
- [ ] `CHANGELOG.md` updated under `## [Unreleased]`, if user-visible
- [ ] Documentation updated, if a promise changed

## If this touches the guarantees

The [invariants](https://github.com/ctolon/dynamic-config-go/blob/main/docs/concurrency.md)
are the list to check against — publication, last-known-good, reload
ordering, callback isolation, bounded queues, and the rule that no
configuration value reaches a log or an error message.

<!-- Which invariants does this touch, and what proves they still hold? -->
