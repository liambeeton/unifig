# unifig

Declarative configuration for a UniFi Network application (the Controller) on a UniFi Dream Router, from human-readable YAML. See `CONTEXT.md` for the domain glossary and [issue #1](https://github.com/liambeeton/unifig/issues/1) for the v1 spec.

Current state: walking skeleton — `unifig export` prints the site's networks as YAML.

## Usage

```sh
make build                               # builds ./unifig

export UNIFIG_HOST=https://192.168.1.1   # Controller base URL (bare hosts get https:// prepended)
export UNIFIG_API_KEY=...                # Controller API key — see below
export UNIFIG_INSECURE=true              # accept the UDR's self-signed certificate

./unifig export
```

Connection config lives in the environment only — never in the resource YAML, and unifig never writes a secret to disk. A [direnv](https://direnv.net) `.envrc` is the intended workflow.

### Creating an API key

On the UDR: UniFi OS → Control Plane → Admins & Users → your admin → Create API Key. Unifig authenticates with that key only (`X-API-KEY`); it never asks for your admin password. API-key auth on Internal API endpoints is enforced by the UniFi OS layer — see `docs/adr/0003-apikey-auth-os-gate.md`.

## Development

`make` is the task entrypoint — run it with no arguments to list the targets. CI runs these same targets, so local and CI cannot drift.

```sh
make check   # fmt-check + lint + build; needs no Docker
make fmt     # format with gofumpt
make e2e     # the dockerized suite
make run ARGS="export"   # run against a live Controller, credentials from the environment
```

Requirements: Go for `make check`, plus Docker for `make e2e`. `make` installs its own pinned golangci-lint (which carries gofumpt) into `bin/`; the pin lives in the Makefile.

Tests drive the whole tool at the process boundary against a real dockerized Controller (see `docs/adr/0003-apikey-auth-os-gate.md` for the rig design). `go test -short ./...` skips the dockerized suite. The Controller version pin lives in `e2e/rig_test.go` (`defaultControllerImage`) and in the CI matrix.

Rig knobs: `UNIFIG_TEST_CONTROLLER_IMAGE` overrides the pinned Controller image; `UNIFIG_TEST_CONTROLLER_URL` points the suite at an already-running demo-mode Controller for a faster inner loop.
