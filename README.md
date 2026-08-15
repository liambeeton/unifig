# unifig

Declarative configuration for a UniFi Network application (the Controller) on a UniFi Dream Router, from human-readable YAML. See `CONTEXT.md` for the domain glossary and [issue #1](https://github.com/liambeeton/unifig/issues/1) for the v1 spec.

Current state: `unifig export` prints the site's networks as YAML, and `unifig validate` checks a config file offline.

## Usage

```sh
make build                               # builds ./unifig

export UNIFIG_HOST=https://192.168.1.1   # Controller base URL (bare hosts get https:// prepended)
export UNIFIG_API_KEY=...                # Controller API key — see below
export UNIFIG_INSECURE=true              # accept the UDR's self-signed certificate

./unifig export > unifig.yaml            # adopt an already-configured Controller
./unifig validate                        # check unifig.yaml; no Controller needed
```

Connection config lives in the environment only — never in the resource YAML, and unifig never writes a secret to disk. A [direnv](https://direnv.net) `.envrc` is the intended workflow.

### Creating an API key

On the UDR: UniFi OS → Control Plane → Admins & Users → your admin → Create API Key. Unifig authenticates with that key only (`X-API-KEY`); it never asks for your admin password. API-key auth on Internal API endpoints is enforced by the UniFi OS layer — see `docs/adr/0003-apikey-auth-os-gate.md`.

## The config file

One `unifig.yaml` describes the Controller's configuration. `examples/unifig.yaml` is a working starting point; `schema/unifig.schema.json` is the contract.

Sections reconcile as they land, and `validate` is ahead of the rest: `networks` is the section `export` writes today, and `wlans` is validated (including its reference to a network) but not yet planned or applied — see [issue #5](https://github.com/liambeeton/unifig/issues/5).

```sh
./unifig validate                 # checks ./unifig.yaml
./unifig validate path/to/it.yaml
```

Validate is entirely offline — it makes no API calls and needs no `UNIFIG_HOST` or `UNIFIG_API_KEY`. It reports every problem it finds at once, with the line to look at:

```
unifig: unifig.yaml is not valid:
  line 3: networks[0].subnett: unknown field "subnett" — check the spelling against the schema
  line 8: wlans[0].network: no network named "Gest" is defined in this file; defined networks are "Default", "IoT"
```

### Editor autocomplete

Put this modeline at the top of your `unifig.yaml` and any editor with the [YAML Language Server](https://github.com/redhat-developer/yaml-language-server) (VS Code, Neovim, JetBrains) will autocomplete fields and flag mistakes as you type:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/liambeeton/unifig/main/schema/unifig.schema.json
```

Working inside a clone, or offline? Point it at the file instead — the path is relative to the YAML file:

```yaml
# yaml-language-server: $schema=./schema/unifig.schema.json
```

`unifig validate` embeds that same schema file, so what your editor accepts and what unifig accepts are the same thing by construction.

### Secrets: `${ENV_VAR}` interpolation

Any value may contain `${ENV_VAR}` references, which unifig replaces from the environment as it loads the file — so secrets stay out of the committed config. A referenced variable that is not set is a validation error naming it, never a silently empty value. Pair this with [direnv](https://direnv.net) to load them per-project.

Three rules:

- **Values, not keys.** The shape of your file is yours; the environment only fills in values.
- **The result is always text.** `${VLAN}` cannot become the number 20 — that rule is what stops a passphrase of `0900` or `true` being read as a number or a boolean.
- **One pass.** A substituted value is never rescanned, so a secret containing `${...}` is inserted verbatim.

## Development

`make` is the task entrypoint — run it with no arguments to list the targets. CI runs these same targets, so local and CI cannot drift.

```sh
make check   # fmt-check + lint + build + test; needs no Docker
make fmt     # format with gofumpt
make e2e     # the dockerized suite
make run ARGS="export"   # run against a live Controller, credentials from the environment
```

Requirements: Go for `make check`, plus Docker for `make e2e`. `make` installs its own pinned golangci-lint (which carries gofumpt) into `bin/`; the pin lives in the Makefile.

Tests drive the whole tool at the process boundary against a real dockerized Controller (see `docs/adr/0003-apikey-auth-os-gate.md` for the rig design). That suite is `make e2e`; `make test` runs everything else, skipping it. The Controller version pin lives in `e2e/rig_test.go` (`defaultControllerImage`) and in the CI matrix.

Validate's tests are the exception, and deliberately so: it is offline by design, and requiring Docker to prove no Controller is needed would be an odd way to demonstrate it. They sit at the highest Docker-free seam instead — `cli.Run`, driven from an external test package in `internal/cli/` so they cannot reach past it.

Rig knobs: `UNIFIG_TEST_CONTROLLER_IMAGE` overrides the pinned Controller image; `UNIFIG_TEST_CONTROLLER_URL` points the suite at an already-running demo-mode Controller for a faster inner loop.
