# unifig's task entrypoint: one obvious command to format, lint, build and
# test. CI calls these same targets, so local and CI cannot drift.
#
# The split that matters: `check` needs no Docker daemon, `e2e` does.

GO ?= go
TOOLS_DIR := bin
BINARY := unifig

# The one pinned tool. golangci-lint carries gofumpt as a formatter, so this is
# the only version to keep in step between local and CI. Bumping it changes the
# installed path below, so the new version is fetched automatically.
GOLANGCI_LINT_VERSION := v2.12.2

# The Go release is part of the pin, because golangci-lint type-checks with the
# go/types it was compiled against. A binary built by an older Go cannot read a
# newer Go's stdlib: upgrading to 1.27 made every run fail in math/rand/v2 with
# "method must have no type parameters", generic methods being a 1.27 addition.
# So the toolchain goes in the path for the same reason the version does — a Go
# upgrade changes the path, and the rebuild happens without anyone noticing.
GO_VERSION := $(shell $(GO) env GOVERSION)
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(GO_VERSION)

# Arguments forwarded to the binary by `run`, e.g. make run ARGS="export".
ARGS ?=

# Where a matrix run keeps what each Controller version's suite did. The
# compatibility table is generated from these, so they are results rather than
# logs; gitignored, because what gets committed is the table.
MATRIX ?= .matrix

# One version of the matrix, for `make matrix-run` — which is how CI runs the
# suite, one version per job. Empty here on purpose: a version that defaulted to
# something would be a job quietly testing the wrong Controller.
VERSION ?=

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@awk 'BEGIN { FS = ":.*##"; print "usage: make <target>\n\ntargets:" } \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format the tree with gofumpt
	$(GOLANGCI_LINT) fmt

.PHONY: fmt-check
fmt-check: $(GOLANGCI_LINT) ## Fail if anything is unformatted, printing the diff
	$(GOLANGCI_LINT) fmt --diff

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Lint the tree
	$(GOLANGCI_LINT) run ./...

.PHONY: build
build: ## Build cmd/unifig
	$(GO) build -o $(BINARY) ./cmd/unifig

# -short skips the dockerized suite (see e2e/main_test.go), so this stays
# inside `check`. It is a no-op while every test lives in e2e/, and it is what
# stops the first internal/ unit test from being silently unrun.
.PHONY: test
test: ## Run the tests that need no Docker
	$(GO) test -short ./...

.PHONY: check
check: fmt-check lint build test ## Everything that does not need Docker

# -count=1 defeats Go's test cache. This suite's result depends on a live
# Controller container, which the cache cannot see, so a cached "ok" would
# assert nothing about the pinned image — and CI restores the build cache
# between runs, which is exactly where that stale pass came from.
.PHONY: e2e
e2e: ## Run the testcontainers suite against a real Controller (needs Docker)
	$(GO) test ./e2e/... -timeout 20m -count=1

# The compatibility matrix: the same suite, against every Controller version in
# compatibility.yaml, and the published table generated from what those runs
# did. Adding a version is a line in that file — none of this changes.
.PHONY: matrix
matrix: matrix-run-all matrix-generate ## Run the suite against every Controller version and regenerate the table (needs Docker)

# One version of it. CI runs this in a job per version and keeps $(MATRIX) as an
# artifact, which the compatibility job then generates the table from.
.PHONY: matrix-run-all
matrix-run-all:
	$(GO) run ./tools/compat run -out $(MATRIX)

.PHONY: matrix-run
matrix-run: ## Run the suite against one Controller version (VERSION=10.5.67)
	@[ -n "$(VERSION)" ] || { echo "VERSION is not set, e.g. make matrix-run VERSION=10.5.67" >&2; exit 1; }
	$(GO) run ./tools/compat run -version $(VERSION) -out $(MATRIX)

.PHONY: matrix-generate
matrix-generate: ## Regenerate the table from results already in $(MATRIX)
	$(GO) run ./tools/compat generate -results $(MATRIX)

.PHONY: matrix-check
matrix-check: ## Fail if the committed table is not what those runs produce
	$(GO) run ./tools/compat check -results $(MATRIX)

# Connection config comes from the environment only — the developer's direnv
# .envrc, which is gitignored. Nothing here defaults, hardcodes or echoes a
# value: the recipe is silent and reads the variables in the shell ($$) rather
# than interpolating them, so a key cannot leak into make's output.
# UNIFIG_INSECURE is deliberately unguarded; it is optional and defaults off.
.PHONY: require-connection
require-connection:
	@[ -n "$$UNIFIG_HOST" ] || { echo "UNIFIG_HOST is not set" >&2; exit 1; }
	@[ -n "$$UNIFIG_API_KEY" ] || { echo "UNIFIG_API_KEY is not set" >&2; exit 1; }

.PHONY: run
run: require-connection build ## Run unifig against a live Controller, e.g. make run ARGS="export"
	@./$(BINARY) $(ARGS)

# Read-only against the router: one GET per recorded file and nothing else. It scrubs what it
# reads before writing it (tools/record-udr/scrub.go), and stops to make the
# operator read the diff. See e2e/testdata/udr/README.md.
.PHONY: record-udr
record-udr: require-connection ## Re-record e2e/testdata/udr from a real UDR
	@$(GO) run ./tools/record-udr

$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_DIR)
	@echo "installing golangci-lint $(GOLANGCI_LINT_VERSION)"
	@GOBIN=$(CURDIR)/$(TOOLS_DIR) $(GO) install \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS_DIR)/golangci-lint $@
