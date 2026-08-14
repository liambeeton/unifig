# unifig's task entrypoint: one obvious command to format, lint, build and
# test. CI calls these same targets, so local and CI cannot drift.
#
# The split that matters: `check` needs no Docker daemon, `e2e` does.

GO ?= go
BIN := bin
BINARY := unifig

# The one pinned tool. golangci-lint carries gofumpt as a formatter, so this is
# the only version to keep in step between local and CI. Bumping it changes the
# installed path below, so the new version is fetched automatically.
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

# Booting the Controller image dominates the e2e suite's runtime.
E2E_TIMEOUT ?= 20m

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

.PHONY: check
check: fmt-check lint build ## Everything that does not need Docker

.PHONY: e2e
e2e: ## Run the testcontainers suite against a real Controller (needs Docker)
	$(GO) test ./e2e/... -timeout $(E2E_TIMEOUT)

# Connection config comes from the environment only — the developer's direnv
# .envrc, which is gitignored. Nothing here defaults, hardcodes or echoes a
# value: the recipe is silent and reads the variables in the shell ($$) rather
# than interpolating them, so a key cannot leak into make's output.
.PHONY: run
run: build ## Run unifig against a live Controller, e.g. make run ARGS="export"
	@[ -n "$$UNIFIG_HOST" ] || { echo "make run: UNIFIG_HOST is not set" >&2; exit 1; }
	@[ -n "$$UNIFIG_API_KEY" ] || { echo "make run: UNIFIG_API_KEY is not set" >&2; exit 1; }
	@./$(BINARY) $(ARGS)

$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	@echo "installing golangci-lint $(GOLANGCI_LINT_VERSION)"
	@GOBIN=$(CURDIR)/$(BIN) $(GO) install \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(BIN)/golangci-lint $@
