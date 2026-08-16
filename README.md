# unifig

Declarative configuration for a UniFi Network application (the Controller) on a UniFi Dream Router, from human-readable YAML. See `CONTEXT.md` for the domain glossary and [issue #1](https://github.com/liambeeton/unifig/issues/1) for the v1 spec.

Current state: networks, WLANs and WAN slots reconcile end to end — `unifig plan` and `unifig apply`, with WLAN passphrases and PPPoE credentials supplied by `${ENV_VAR}` — alongside `unifig export` to adopt a configured Controller and `unifig validate` to check a config file offline.

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

Sections reconcile as they land. `networks`, `wlans` and `wan` are the sections every verb handles today; the rest of the v1 catalogue — Encrypted DNS, firewall Zones and Policies, port forwards, DHCP reservations — follows.

A WLAN names the network its clients join, and that reference is checked offline:

```yaml
networks:
  - name: IoT
    vlan: 20
    subnet: 10.20.0.1/24

wlans:
  - name: Home IoT
    network: IoT                       # must be a network defined above
    passphrase: ${WIFI_IOT_PASSPHRASE} # never the passphrase itself — see below

wan:
  - slot: WAN                          # the router's own name for the uplink
    type: pppoe
    username: ${WAN_PPPOE_USERNAME}
    password: ${WAN_PPPOE_PASSWORD}
```

### WAN slots are Settings, not Resources

Your router has the uplinks it has. So `wan` entries are matched by `slot` — `WAN`, `WAN2`, `WAN_LTE_FAILOVER`, the Controller's own names, whatever you renamed the connection to in the UI — and unifig only ever **updates** one:

- **It never creates a slot.** Naming a slot your Controller does not have is an error that lists the ones it does, not a request to invent an uplink.
- **It never deletes or prunes one.** A slot your file leaves out is one unifig does not manage; `--prune` does not see it.
- **Every WAN change is confirmed on its own** before it is applied — see [Risky changes](#risky-changes).

`type` is `dhcp`, `pppoe` or `disabled`. Static addressing, DS-Lite and MAP-E are not modelled yet, so a slot configured that way is left to the Controller entirely: `export` writes it as `- slot: WAN` and says so, and unifig changes nothing about how it connects.

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

### Secrets, once they are loaded

Interpolation keeps a secret out of the file. What keeps it out of everything else is that unifig treats a secret field as secret however it arrived — a passphrase typed straight into the YAML is redacted exactly like one that came from the environment.

- **Plans never print one.** A changing passphrase shows as `passphrase: (hidden)`; `plan --json` writes `{"name": "passphrase", "from": null, "to": null, "secret": true}`. The field is there because it is changing, so a pipeline can gate on it, and the value is not.
- **Validation messages never print one.** A passphrase outside the Controller's own 8-to-64-character bound is reported as a length, never as the value.
- **Export redacts by default.** See [Export](#export).

The modelled secrets today are a WLAN's `passphrase` and a WAN slot's PPPoE `password`, and both behave identically.

Where a secret does have to be readable, it is because reconcile could not work otherwise: the Controller hands a WLAN's passphrase and a slot's PPPoE password back in the clear, which is what lets unifig tell "already correct" from "needs rotating" with no state file (`docs/adr/0007-secrets-read-back-so-they-diff-normally.md`).

## Plan and apply

`plan` reads the Controller, compares it to your file and prints what it would change. It changes nothing.

```
  + network "Guest"
      vlan:   30
      subnet: 192.168.30.1/24

  + wlan "Home Guest"
      network:    Guest
      passphrase: (hidden)

  ~ network "IoT"
      subnet: 10.20.0.1/24 -> 192.168.20.1/24

Plan: 2 to create, 1 to update.
```

`apply` does the same and then executes it, after asking:

```sh
./unifig apply                 # prints the plan, then asks
./unifig apply --auto-approve  # for a pipeline, or an operator who has already looked
```

### Risky changes

A change that can cut this site off the internet — anything touching a WAN slot — is confirmed **on its own**, even under `--auto-approve`. The plan says so before you approve anything:

```
  ~ wan "WAN"
      type:     dhcp -> pppoe
      username: (none) -> isp-user
      password: (hidden)
      ! this is the site's internet uplink, and changing it can cut the connection until the new settings work
```

and apply asks about that one change before making it:

```
Risky change: ~ wan "WAN"
  this is the site's internet uplink, and changing it can cut the connection until the new settings work.
Apply it? [y/N]
```

Saying no skips that change and applies the rest — the question was about that change, not about your file. Nothing is ever hard-blocked: `--allow-risky` says yes in advance, which is what an unattended pipeline needs (`./unifig apply --auto-approve --allow-risky`). Without it, an apply with nobody to answer leaves the Risky change unapplied and says so, and the next `plan` still exits 2.

`plan --json` carries the same sentence as `"risk"` on the change, so a pipeline can gate on the dangerous ones without keeping a list of which kinds those are. The reasoning is in `docs/adr/0009-risky-changes-are-confirmed-one-at-a-time.md`.

There is no state file. Every run diffs your file against the live Controller directly (see `docs/adr/0001-stateless-reconcile.md`), which means:

- **Resources are matched by name.** Controller IDs never appear in your file and are never needed. Renaming a network in YAML means replacing it, and two live networks sharing a name is an error rather than a guess.
- **Nothing is destroyed implicitly.** A network the Controller has and your file does not is left alone. Deleting it is [`--prune`](#pruning)'s job, and you have to ask.
- **Only the fields your file models are written.** An apply that changes a subnet leaves the DHCP server, DNS entries and everything else on that network exactly as you set them in the Controller. The single exception announces itself in the plan: a DHCP pool cannot stay in a subnet the network no longer has, so a subnet change that strands one moves it.
- **A field you leave out is a field unifig doesn't manage.** Only `name` is required, so `- name: IoT` is a complete entry meaning "this network is mine to match, and I'm not managing anything about it yet". It never means "clear its VLAN and subnet" — clearing a value is something you do in the Controller.
- **Changes run in dependency order.** A network is created before the WLAN that joins it and a WLAN is deleted before the network it was on, so a file that declares both applies in one run. The plan prints the order apply will use, so you see it before you agree to it.
- **A network unifig creates is a working LAN.** Your file names three fields; a networkconf has a hundred. The rest come from the Controller's own defaults for a new LAN — NAT and DHCP on, a pool from the sixth address to the last. Those are set on create only, so anything you change afterwards is yours and stays. The same goes for a WLAN: it is enabled, on the default AP group, and everything else is the Controller's own choice.

### When an apply stops partway

There is no rollback, so apply's contract is that it tells you exactly where it got to. It stops at the first error, having attempted nothing after it:

```
  + network "Guest" created

Applied 1 of 3 changes. These were not applied:
  + wlan "Home Guest"
  - network "Old Lab"

Nothing was rolled back; apply is safe to run again once this is fixed.
```

The error itself goes to stderr, and the exit code is 1. Re-running *is* the recovery: the next plan is computed from the Controller as it now stands, including the part that succeeded, so a fixed-and-re-run apply picks up exactly where this one stopped without being told anything about it.

### Pruning

By default your file is a list of what unifig manages, not a list of everything that may exist. `--prune` makes it the second thing: every Resource of a managed type that the file does not name becomes a deletion.

```sh
./unifig plan --prune    # see what would go, change nothing
./unifig apply --prune   # print the plan, ask, then delete
```

Deletions appear in the plan like any other change, showing what was in the network so you can recognise it before agreeing to lose it. They come last — after the creates and updates — because apply stops at the first failure: a half-finished apply leaves you missing changes rather than missing networks.

```
  + network "Guest"
      vlan:   30
      subnet: 192.168.30.1/24

  - network "Old Lab"
      vlan:   90
      subnet: 10.90.0.1/24

Plan: 1 to create, 1 to delete.
```

Four things `--prune` will not do:

- **Reach a section your file doesn't have.** A file with no `wlans:` key says nothing about WLANs, so prune deletes none of them — the same rule as an omitted field, one level up (see `docs/adr/0006-prune-reaches-only-the-sections-the-file-has.md`). Write `wlans: []` to say there should be none; that is a statement, and prune acts on it.
- **Delete what the Controller says it owns.** The built-in Default network is marked undeletable on the Controller itself, and unifig reads that marker rather than keeping a list of names (see `docs/adr/0005-builtin-exemption-from-the-controller.md`). It is never pruned, whether or not your file names it.
- **Touch anything unifig does not manage.** WAN slots share a collection with your LANs; they are Settings, not Resources, so unifig updates them and never deletes one, whether or not your file names them. Nor does prune see a WLAN attached to something that isn't one of your LANs — unifig has no name to write for it, so it can't be exported either, and the two go together on purpose: a WLAN adoption couldn't describe is not one prune may delete.
- **Persist.** The flag applies to the run you passed it to. There is no state file, so nothing remembers it.

## Export

`export` reads the Controller and writes the config that describes it — the way to adopt a router you have already configured by hand, without transcribing anything. What it writes is config the other verbs accept, by construction: `plan`, `apply` and `export` share one answer about which live Resources are in scope and what each looks like in YAML, so a freshly exported file plans clean.

```sh
./unifig export > unifig.yaml       # stdout by default
./unifig export -o unifig.yaml      # or straight to a file, created 0600
./unifig export --with-secrets      # passphrases in plaintext instead of redacted
```

Secrets are redacted to `${ENV_VAR}` references unless you ask otherwise, because export's output goes into a git repository and a file that arrives with a live passphrase in it has already been committed by the time anyone notices. The variables it invented are listed on stderr, so `export > unifig.yaml` still tells you what to set while stdout carries nothing but YAML:

```
$ ./unifig export > unifig.yaml

Redacted 1 secret. Set it before running unifig:

  export UNIFIG_WLAN_HOME_IOT_PASSPHRASE=...

The values are on the Controller; `unifig export --with-secrets` prints them inline instead.
```

An export that fails prints no YAML and writes no file — so pointing `-o` at your existing `unifig.yaml` cannot lose it to an unreachable Controller or a bad key.

If your Controller holds something unifig can't describe in config, export says so rather than quietly coming up short — a WLAN attached to anything that isn't one of your LANs has no network for unifig to name:

```
Left out 1 WLAN, which unifig does not manage: "Guest Portal".
Each is attached to something that is not one of this site's LANs, so there is
no network for unifig to name in the config. It manages nothing about them, and
`--prune` will not delete them.
```

A WAN slot that connects in a way unifig does not model gets the other half of the same promise: the slot is in the file so you can see it exists, with nothing under it and a notice saying why.

```
Wrote 1 WAN slot with nothing but the slot: "WAN2".
Each connects in a way unifig does not model — static addressing, for example —
so there is nothing for the config to say about it. unifig will match the slot
and change nothing about how it connects.
```

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
      "kind": "network",
      "name": "IoT",
      "fields": [
        { "name": "subnet", "from": "10.20.0.1/24", "to": "192.168.20.1/24" }
      ]
    },
    {
      "action": "update",
      "kind": "wlan",
      "name": "Home IoT",
      "fields": [
        { "name": "passphrase", "from": null, "to": null, "secret": true }
      ]
    },
    {
      "action": "update",
      "kind": "wan",
      "name": "WAN",
      "fields": [
        { "name": "type", "from": "dhcp", "to": "pppoe" },
        { "name": "password", "from": null, "to": null, "secret": true }
      ],
      "risk": "this is the site's internet uplink, and changing it can cut the connection until the new settings work"
    }
  ]
}
```

`kind` is the managed type — `network`, `wlan`, `wan` — covering both the Resources unifig creates and deletes and the Settings it only updates. `risk` is present only on a change that needs individual confirmation.

Changes are listed in the order apply will run them, so a consumer reading the array is reading the sequence.

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

WAN slots are the one thing that container cannot stand in for — with no gateway it has no WAN entries at all — so those tests run against recorded Controller responses served at the same base URL, through the same API-key header, to the same real binary (`e2e/replay_test.go`). The recordings live in `e2e/testdata/udr/`, along with the one command that re-records them from a real router; `docs/adr/0008-wan-slots-replay-recorded-responses.md` explains the design and what it does not yet prove.

Validate's tests are the exception, and deliberately so: it is offline by design, and requiring Docker to prove no Controller is needed would be an odd way to demonstrate it. They sit at the highest Docker-free seam instead — `cli.Run`, driven from an external test package in `internal/cli/` so they cannot reach past it.

Rig knobs: `UNIFIG_TEST_CONTROLLER_IMAGE` overrides the pinned Controller image; `UNIFIG_TEST_CONTROLLER_URL` points the suite at an already-running demo-mode Controller for a faster inner loop.
