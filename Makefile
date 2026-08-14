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
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

# Arguments forwarded to the binary by `run`, e.g. make run ARGS="export".
ARGS ?=

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

# Connection config comes from the environment only — the developer's direnv
# .envrc, which is gitignored. Nothing here defaults, hardcodes or echoes a
# value: the recipe is silent and reads the variables in the shell ($$) rather
# than interpolating them, so a key cannot leak into make's output.
# UNIFIG_INSECURE is deliberately unguarded; it is optional and defaults off.
.PHONY: require-connection
require-connection:
	@[ -n "$$UNIFIG_HOST" ] || { echo "make run: UNIFIG_HOST is not set" >&2; exit 1; }
	@[ -n "$$UNIFIG_API_KEY" ] || { echo "make run: UNIFIG_API_KEY is not set" >&2; exit 1; }

.PHONY: run
run: require-connection build ## Run unifig against a live Controller, e.g. make run ARGS="export"
	@./$(BINARY) $(ARGS)

$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_DIR)
	@echo "installing golangci-lint $(GOLANGCI_LINT_VERSION)"
	@GOBIN=$(CURDIR)/$(TOOLS_DIR) $(GO) install \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS_DIR)/golangci-lint $@
