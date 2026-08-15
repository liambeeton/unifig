# unifig

Declarative configuration for a UniFi Network application (the Controller) on a UniFi Dream Router, from human-readable YAML. See `CONTEXT.md` for the domain glossary and [issue #1](https://github.com/liambeeton/unifig/issues/1) for the v1 spec.

Current state: networks reconcile end to end — `unifig plan` and `unifig apply` — alongside `unifig export` to adopt a configured Controller and `unifig validate` to check a config file offline.

## Usage

```sh
make build                               # builds ./unifig

export UNIFIG_HOST=https://192.168.1.1   # Controller base URL (bare hosts get https:// prepended)
export UNIFIG_API_KEY=...                # Controller API key — see below
export UNIFIG_INSECURE=true              # accept the UDR's self-signed certificate

./unifig export > unifig.yaml            # adopt an already-configured Controller
./unifig validate                        # check unifig.yaml; no Controller needed
./unifig plan                            # show what would change
./unifig apply                           # change it, after asking
```

Connection config lives in the environment only — never in the resource YAML, and unifig never writes a secret to disk. A [direnv](https://direnv.net) `.envrc` is the intended workflow.

### Creating an API key

On the UDR: UniFi OS → Control Plane → Admins & Users → your admin → Create API Key. Unifig authenticates with that key only (`X-API-KEY`); it never asks for your admin password. API-key auth on Internal API endpoints is enforced by the UniFi OS layer — see `docs/adr/0003-apikey-auth-os-gate.md`.

## The config file

One `unifig.yaml` describes the Controller's configuration. `examples/unifig.yaml` is a working starting point; `schema/unifig.schema.json` is the contract.

Sections reconcile as they land, and `validate` is ahead of the rest: `networks` is the section `plan`, `apply` and `export` handle today, and `wlans` is validated (including its reference to a network) but not yet planned or applied — see [issue #5](https://github.com/liambeeton/unifig/issues/5).

Every verb that reads config takes an optional file argument, defaulting to `./unifig.yaml`:

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

## Plan and apply

`plan` reads the Controller, compares it to your file and prints what it would change. It changes nothing.

```
  + network "Guest"
      vlan:   30
      subnet: 192.168.30.1/24

  ~ network "IoT"
      subnet: 10.20.0.1/24 -> 192.168.20.1/24

Plan: 1 to create, 1 to update.
```

`apply` does the same and then executes it, after asking:

```sh
./unifig apply                 # prints the plan, then asks
./unifig apply --auto-approve  # for a pipeline, or an operator who has already looked
```

There is no state file. Every run diffs your file against the live Controller directly (see `docs/adr/0001-stateless-reconcile.md`), which means:

- **Resources are matched by name.** Controller IDs never appear in your file and are never needed. Renaming a network in YAML means replacing it, and two live networks sharing a name is an error rather than a guess.
- **Nothing is destroyed implicitly.** A network the Controller has and your file does not is left alone. Deleting is `--prune`'s job, and `--prune` does not exist yet ([issue #4](https://github.com/liambeeton/unifig/issues/4)).
- **Only the fields your file models are written.** An apply that changes a subnet leaves the DHCP server, DNS entries and everything else on that network exactly as you set them in the Controller. The single exception announces itself in the plan: a DHCP pool cannot stay in a subnet the network no longer has, so a subnet change that strands one moves it.
- **A field you leave out is a field unifig doesn't manage.** Only `name` is required, so `- name: IoT` is a complete entry meaning "this network is mine to match, and I'm not managing anything about it yet". It never means "clear its VLAN and subnet" — clearing a value is something you do in the Controller.
- **Re-running is the recovery.** Apply stops at the first error and reports what it got through; the next plan is computed from the Controller as it now stands, so a fixed-and-re-run apply picks up exactly where it stopped.
- **A network unifig creates is a working LAN.** Your file names three fields; a networkconf has a hundred. The rest come from the Controller's own defaults for a new LAN — NAT and DHCP on, a pool from the sixth address to the last. Those are set on create only, so anything you change afterwards is yours and stays.

### In a pipeline

`plan` distinguishes its outcomes in its exit code, so a CI job or a git hook can gate on drift without reading the output:

| Exit | Meaning |
| ---- | ------- |
| `0`  | the Controller already matches the config |
| `1`  | an error — bad config, unreachable Controller, bad key |
| `2`  | changes are pending |

`plan --json` writes the same changes for a machine. An empty plan is `{"changes": []}`, never a null:

```json
{
  "changes": [
    {
      "action": "update",
      "resource": "network",
      "name": "IoT",
      "fields": [
        { "name": "subnet", "from": "10.20.0.1/24", "to": "192.168.20.1/24" }
      ]
    }
  ]
}
```

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
