# Everything CI runs, runnable locally. `make check` is the gate.

GO ?= go
FUZZTIME ?= 30s
BENCHTIME ?= 200ms

.PHONY: all
all: check

## check: the full gate — format, vet, lint, race tests, fuzz smoke, vulnerabilities
.PHONY: check
check: fmt-check vet staticcheck test-race fuzz-smoke vuln

## test: run every test
.PHONY: test
test:
	$(GO) test ./...

## test-race: run every test under the race detector
.PHONY: test-race
test-race:
	$(GO) test -race -count=1 ./...

## bench: run the benchmarks
.PHONY: bench
bench:
	$(GO) test -run '^$$' -bench . -benchmem -benchtime $(BENCHTIME) ./benchmarks/

## bench-compile: make sure the benchmarks still build, without running them
.PHONY: bench-compile
bench-compile:
	$(GO) test -run '^$$' -bench . -benchtime 1x ./benchmarks/

## fuzz-smoke: run each fuzz target briefly, as CI does on every push
.PHONY: fuzz-smoke
fuzz-smoke:
	$(GO) test -run '^$$' -fuzz FuzzReloadDocument -fuzztime $(FUZZTIME) .
	$(GO) test -run '^$$' -fuzz FuzzSubscriberOperations -fuzztime $(FUZZTIME) .

## fmt: format the tree
.PHONY: fmt
fmt:
	gofmt -w -s .

## fmt-check: fail if anything is unformatted
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l -s .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-ed:"; echo "$$unformatted"; exit 1; \
	fi

## vet: run go vet
.PHONY: vet
vet:
	$(GO) vet ./...

## staticcheck: run staticcheck
.PHONY: staticcheck
staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

## vuln: scan dependencies for known vulnerabilities
.PHONY: vuln
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

## tidy: tidy the module
.PHONY: tidy
tidy:
	$(GO) mod tidy

## examples: build every example
.PHONY: examples
examples:
	$(GO) build -o /dev/null ./examples/...

## help: list the targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
