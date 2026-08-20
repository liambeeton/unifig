# unifig

Declarative configuration for a UniFi Network application (the Controller) on a UniFi Dream Router, from human-readable YAML. See `CONTEXT.md` for the domain glossary and [issue #1](https://github.com/liambeeton/unifig/issues/1) for the v1 spec.

Current state: all seven config areas reconcile end to end — networks and VLANs, WLANs, firewall zones and policies, port forwards, DHCP reservations, WAN slots and Encrypted DNS — through `unifig plan` and `unifig apply`, with WLAN passphrases, PPPoE credentials and DNS stamps supplied by `${ENV_VAR}`, alongside `unifig export` to adopt a configured Controller and `unifig validate` to check a config file offline.

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

### Which Controller versions this works on

`docs/COMPATIBILITY.md` is the answer, and it is generated rather than written: CI runs the whole test suite against every UniFi Network version in `compatibility.yaml`, and the table is built from what those runs did. It also says which areas a container cannot answer for at all, and which Controller version the recorded responses behind them came from. The versions are deliberately not repeated here — the table is the one place they live, and a list in this file is a list that goes stale.

A version that is not in the table gets a warning and nothing else:

```
unifig: this Controller runs UniFi Network 10.6.0, which is newer than any version unifig has been
tested against — the newest is 10.5.67 (docs/COMPATIBILITY.md). Carrying on anyway.
```

(That is an illustration; `docs/COMPATIBILITY.md` prints the same sentence with the versions this build actually carries.)

Every online command prints it once, on stderr, and then does exactly what it was asked. Untested means nobody has run the suite against it, which is not the same as broken — unifig has no business refusing to manage a router on that basis.

## The config file

One `unifig.yaml` describes the Controller's configuration. `examples/unifig.yaml` is a working starting point; `schema/unifig.schema.json` is the contract.

`networks`, `wlans`, `zones`, `firewall-policies`, `port-forwards`, `dhcp-reservations`, `wan` and `encrypted-dns` are the sections every verb handles — the whole v1 catalogue.

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

zones:
  - name: Untrusted
    networks: [IoT]                    # the whole membership, not an addition

firewall-policies:
  - name: IoT to internet
    action: allow                      # allow, block or reject
    source: Untrusted
    destination: External              # a zone the Controller already has

port-forwards:
  - name: Home Assistant
    port: 8443                         # what answers on the internet side
    forward-ip: 10.20.0.10             # an address, not a network in this file
    forward-port: 8123
    protocol: tcp                      # tcp, udp or tcp_udp
    source: 203.0.113.0/24             # omit to accept traffic from anywhere

dhcp-reservations:
  - mac: "00:1a:2b:3c:4d:5e"           # the client, and the whole of its identity
    ip: 10.20.0.50                     # no network to name — see below

wan:
  - slot: WAN                          # the router's own name for the uplink
    type: pppoe
    username: ${WAN_PPPOE_USERNAME}
    password: ${WAN_PPPOE_PASSWORD}

encrypted-dns:                         # a mapping, not a list: there is one
  state: custom
  servers:
    - name: Quad9
      stamp: ${DNS_QUAD9_STAMP}      # a stamp is a secret — see below
```

### WAN slots are Settings, not Resources

Your router has the uplinks it has. So `wan` entries are matched by `slot` — `WAN`, `WAN2`, `WAN_LTE_FAILOVER`, the Controller's own names, whatever you renamed the connection to in the UI — and unifig only ever **updates** one:

- **It never creates a slot.** Naming a slot your Controller does not have is an error that lists the ones it does, not a request to invent an uplink.
- **It never deletes or prunes one.** A slot your file leaves out is one unifig does not manage; `--prune` does not see it.
- **Every WAN change is confirmed on its own** before it is applied — see [Risky changes](#risky-changes).

`type` is `dhcp`, `pppoe` or `disabled`. Static addressing, DS-Lite and MAP-E are not modelled yet, so a slot configured that way is left to the Controller entirely: `export` writes it as `- slot: WAN` and says so, and unifig changes nothing about how it connects.

### Encrypted DNS is a Setting too, and there is exactly one

`encrypted-dns` is what UniFi's UI calls DNS Shield. It is a Setting like the WAN slots — only ever updated, never created, deleted or pruned — and a **singleton**: a Controller has one, so the section is a mapping rather than a list, there is nothing to match it by, and a plan line about it carries no name:

```
  ~ encrypted-dns
      state:          off -> custom
      servers:        (none) -> "Quad9"
      server "Quad9": (hidden)
```

`state` is `off`, `auto`, `manual` or `custom`. `manual` picks from the Controller's built-in providers, which unifig does not model — so it says which mode to be in and leaves the choice of providers to the UI.

**`servers` is one field, not a list of Resources.** That is the one place in unifig where deleting a line from your file changes something on the Controller, so it is worth being precise about:

- **Leave `servers` out** and unifig does not manage the list at all — your resolvers stay exactly as they are, even while unifig changes the `state` around them.
- **State `servers`** and that list *is* the Controller's list. A resolver you stop naming is removed on the next apply — announced in the plan first, as `servers: "Quad9", "Cloudflare" -> "Quad9"`, so nothing goes unless you approve the line that says it is going.
- **Write `servers: []`** to say there should be none. This is what `export` writes for a Controller with no custom resolvers, so an exported file always states the list rather than trailing off.

A resolver's `name` is its natural key within the section, so two cannot share one — in your file, where `validate` catches it offline, or on the Controller, where unifig stops rather than guess which of the two you meant.

A resolver already on the Controller keeps everything unifig does not model, including whether it is switched on: rotating a stamp does not re-enable a resolver you turned off in the UI. One unifig adds is enabled, because a resolver stored and ignored is a config line that does nothing.

Changing encrypted DNS is **not** a [Risky change](#risky-changes), and the line is deliberate: a bad stamp breaks name resolution, but the uplink stays up, the Controller stays reachable, and the fix is one field away. Risky is reserved for changes whose recovery could need physical access (`docs/adr/0012-encrypted-dns-is-a-singleton-setting.md`).

### Zones and firewall policies

A Zone groups networks; a Firewall Policy governs the traffic between a pair of Zones. Both are Resources — a Zone matched by name, a Policy by its name together with the pair of Zones it governs — and apply in dependency order — a file declaring a network, a Zone holding it and a Policy governing that Zone works in one run.

The Controller ships its **own** Zones (`External`, `Internal`, `Gateway`, …), and this is the first place that matters:

- **You can name a built-in Zone and manage what is in it.** `- name: External` with a `networks:` list is an ordinary update — including the Zones the Controller's own UI marks uneditable, which take a membership change over the API like any other (`docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md`).
- **unifig never creates or deletes one**, `--prune` included. That exemption is read off the marker the Controller puts on the object, not from a list of names unifig keeps — so it stays correct on firmware this project has never seen (`docs/adr/0005-builtin-exemption-from-the-controller.md`).
- **A Policy may name a Zone that is not in your file**, which is the common case: `destination: External` needs no `zones:` entry. The name is resolved against the Controller when unifig reads it, and a Zone that exists nowhere is reported with the Zones your site actually has.

A Zone's `networks` follows the same rule as `servers` above — stating it states the whole membership — with one addition that is easy to get wrong and would be expensive:

```
  ~ zone "External"
      networks: (none) -> "IoT"
                this zone also holds something that is not one of this site's LANs, which unifig does not name or change
```

The built-in `External` Zone holds your WAN, and your config has no name for a WAN network. unifig therefore manages the members it *can* name and leaves the rest exactly where they are: stating the LANs in `External` does not detach your uplink from it. The plan says so, because a membership showing only part of the truth would otherwise read as one that empties the Zone.

**A network is in exactly one Zone, and the Controller is the one enforcing that.** Putting a network in a second Zone takes it out of the first — one request, two Zones changed, and neither unifig nor you asked for the second. Taking a network out of a Zone does not leave it outside every Zone either: the Controller moves it to `Internal`. So a plan that changes a membership names both sides of it (`docs/adr/0020-a-network-lives-in-exactly-one-zone.md`):

```
  ~ zone "Hotspot"
      networks: "Guest" -> "Guest", "Cameras"
                the network "Cameras" is in the zone "Dmz" now, and a network belongs to exactly one zone: applying this takes it out

  ~ zone "Dmz"
      networks: "Cameras" -> (none)
                the network "Cameras" does not end up outside every zone: this plan puts it in the zone "Hotspot"
```

Where nothing in your file claims the network, the second note names `Internal` instead — the Zone the Controller falls back to, found by the Controller's own key for it rather than by that name. And because a network can only be in one Zone, your file may not put one in two: `validate` catches that offline, naming the Zone that already has it.

A `--prune` that deletes a Zone says the same thing one step short: the networks it held are not deleted with it and end up in some other Zone, but *which* one has never been measured for a deletion, so unifig says so rather than naming `Internal` on a hunch.

A Policy states its verdict and both ends, always — `action`, `source` and `destination` are required, because a policy that allowed or blocked nothing in particular is not a policy. Everything else about it (ports, addresses, schedules, logging) is the Controller's, and survives an apply untouched.

### Port forwards

`port-forwards` is the record of what your network answers to from the internet, which is what shapes the section: everything but `source` is required, because a forward that did not say which port, which host and which protocol would not describe an exposed service.

```
  + port-forward "Home Assistant"
      port:         8443
      forward-ip:   10.20.0.10
      forward-port: 8123
      protocol:     tcp
      source:       any
                    the config states no source, so this forward accepts traffic from anywhere on the internet
```

Three things to know about it:

- **`forward-ip` is an address, not a reference.** It names a host rather than a network in your file, so nothing checks it offline and nothing about a forward holds a network back from `--prune`. A host that moves is a change to make here.
- **`source` omitted means unmanaged**, as everywhere else: unifig leaves whatever the Controller has, so a forward you narrowed to one address in the UI keeps that narrowing. It is the field the create above is warning about — a forward unifig creates with no `source` is open to anyone, and the plan says so before it does.
- **Ports are single ports.** The Controller will hold a range (`27015-27020`) or a comma-separated list, and unifig models one port arriving and one port inside. A forward stating otherwise is out of unifig's reach entirely: `export` leaves it out and says so, and `--prune` will not delete it — saying that too, rather than sparing it quietly:

  ```
  ! port-forward: the port forward "Game server" will not be deleted: a port of it
    is a range or a list rather than a single port, which unifig cannot state, so
    export leaves it out of the config and prune leaves it where it is.
  ```

  A forward you *do* name in your file is managed whichever ports the Controller has it on, so narrowing `27015-27020` to `27015` is an ordinary update — the plan shows the range on the losing side.

Everything else a forward carries — which uplink it listens on, whether it logs, whether it is switched on at all — is the Controller's and survives an apply untouched. Opening or closing a port is **not** a [Risky change](#risky-changes): it cannot take the site off the internet or put the Controller out of reach, and the way back is to put the forward where it was.

### DHCP reservations

`dhcp-reservations` pins a client to an address. It is keyed by MAC, and it is the one section that does not describe an object on your Controller at all — which is worth knowing before you point `--prune` at it.

Your Controller keeps a record for **every client it has ever seen**, carrying whatever you put on it in the UI: a name, a note, a user group, whether it is blocked. A reservation is two fields of that record. So:

- **unifig manages the address and nothing else.** Renaming a laptop in the UI and moving its address in YAML do not fight; an apply writes the address and hands the rest of the record straight back.
- **A client with no reserved address is not in scope.** unifig sees the reservations, not your address book. `export` does not write those clients, `--prune` cannot touch them, and `dhcp-reservations: []` gives up every reserved address while leaving every client record alone.
- **Removing a reservation gives the address up — it does not forget the device.** The client keeps its name and its note and takes a dynamic address at its next lease. The plan says so on the line, because `- dhcp-reservation` reads like more than it is:

  ```
  - dhcp-reservation "00:1a:2b:3c:4d:5e"
      ip: 10.20.0.50
           the Controller calls this client "Study NAS", and its record stays exactly
           as it is — only the fixed address is given up, so it takes one from the
           pool instead
  ```

  If you did mean to forget the device, that is a click in the Controller's UI. unifig will not do it from a deleted line of YAML (`docs/adr/0015-a-reservation-is-a-projection-of-a-client-record.md`).

**There is no network to name**, and that is your Controller's design rather than a missing field. It works out which network an address belongs to from the address: one that falls inside no network's subnet is refused, whichever network the record happens to point at. The same rule runs the other way, so a network with an address reserved inside it is one `--prune` holds back:

```
! network: the network "IoT" will not be deleted: this plan leaves the DHCP reservation
  "00:1a:2b:3c:4d:5e" reserving an address inside it.
```

**MAC addresses are matched case-insensitively**, and this is the only key in unifig that is. Your Controller stores every MAC in lower case, so `00:1A:2B:…` in your file matches `00:1a:2b:…` on the Controller, `export` writes lower case, and two entries in one file differing only in case are one reservation written twice — which `validate` reports rather than applying in file order.

Losing a fixed address is **not** a [Risky change](#risky-changes): the client falls back to the DHCP pool, the Controller stays reachable, and the way back is to put the reservation back.

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

The modelled secrets today are a WLAN's `passphrase`, a WAN slot's PPPoE `password` and a custom DNS server's `stamp`, and all three behave identically. A stamp is a secret because one for a private endpoint identifies the account it belongs to.

Where a secret does have to be readable, it is because reconcile could not work otherwise: the Controller hands all three back in the clear, which is what lets unifig tell "already correct" from "needs rotating" with no state file (`docs/adr/0007-secrets-read-back-so-they-diff-normally.md`).

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

- **Resources are matched by a natural key, which is the name in the common case rather than the rule.** A Firewall Policy is keyed by its name *and* the pair of Zones it governs; a DHCP Reservation by MAC. Controller IDs never appear in your file and are never needed. Renaming a network in YAML means replacing it, and two live networks sharing a key is an error rather than a guess.
- **Nothing is destroyed implicitly.** A network the Controller has and your file does not is left alone. Deleting it is [`--prune`](#pruning)'s job, and you have to ask.
- **Only the fields your file models are written.** An apply that changes a subnet leaves the DHCP server, DNS entries and everything else on that network exactly as you set them in the Controller. The single exception announces itself in the plan: a DHCP pool cannot stay in a subnet the network no longer has, so a subnet change that strands one moves it.
- **A field you leave out is a field unifig doesn't manage.** Only `name` is required, so `- name: IoT` is a complete entry meaning "this network is mine to match, and I'm not managing anything about it yet". It never means "clear its VLAN and subnet" — clearing a value is something you do in the Controller.
- **A WLAN's `passphrase` carries its security mode.** On the Controller the two are one decision, so stating a passphrase states WPA-PSK and leaving it out leaves whatever the Controller has — open, WPA or enterprise — alone. It reads the same way round: a WLAN the Controller holds as open exports as a name and a network with no `passphrase:` line, because a passphrase left lying on it from whenever it was last WPA-PSK says nothing about how clients join today. Putting one back is allowed — that is you asking for it — and the plan says the security mode is changing before an apply changes it.
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

### Backing up first

`apply --backup-first` has the Controller write a backup of its own configuration before the first change, and applies nothing at all if it cannot get one:

```
  ~ network "IoT"
      subnet: 10.20.0.1/24 -> 192.168.20.1/24

Plan: 1 to update.

Backed up the Controller first: https://192.168.1.1/proxy/network/dl/backup/10.5.67.unf

  ~ network "IoT" updated

Applied 1 change.
```

It is off unless you ask for it — a backup writes a file on your router — and it is worth asking for on the runs that touch a WAN slot, which is where "run it again" is not much of a recovery.

What the flag does and does not buy you:

- **The Controller takes the backup, and keeps it.** unifig asks, waits for the file to be written — the command answers only once it is — and checks the Controller is serving it back before the first change goes out. It never downloads it: the file stays where the Controller's own UI looks for it.
- **unifig does not restore it.** There is no rollback here and this does not add one. Restoring is a thing you do deliberately, in the Controller's UI, on the day you need it — what this buys is that there is something to restore.
- **A backup it cannot confirm stops the apply.** Nothing is applied, the exit code is 1, and the Controller is exactly as it was.
- **It is one slot per Controller version.** The Controller names the file after its own version and overwrites it each time, so what you have is the site as it was before your most recent `--backup-first` apply, and nothing older. The automatic backups your console keeps are a separate collection and are left alone.
- **Nothing to apply means nothing to back up.** An empty plan, or one whose changes you all refused, leaves the Controller untouched — no backup either.

For an unattended run: `./unifig apply --auto-approve --backup-first`. What a real UDR answered when this was worked out, down to the request and the response, is in `docs/adr/0017-a-backup-can-be-triggered-through-the-internal-api.md`.

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

Six things `--prune` will not do:

- **Reach a section your file doesn't have.** A file with no `wlans:` key says nothing about WLANs, so prune deletes none of them — the same rule as an omitted field, one level up (see `docs/adr/0006-prune-reaches-only-the-sections-the-file-has.md`). Write `wlans: []` to say there should be none; that is a statement, and prune acts on it.
- **Delete what the Controller says it owns.** The built-in Default network is marked undeletable on the Controller itself, and unifig reads that marker rather than keeping a list of names (see `docs/adr/0005-builtin-exemption-from-the-controller.md`). It is never pruned, whether or not your file names it. Each type has its own marker — a network's is `attr_no_delete`, a firewall zone's is `default_zone`, a policy's is `predefined` — and where unifig cannot read one, it deletes nothing of that type and says so rather than guessing:

  ```
  Plan: 1 to create.

  ! zone: no zone will be deleted: unifig could not read which zones the Controller
    marks as its own, and deleting the wrong one would take the site off the internet.
  ```

  That line survives an otherwise-empty plan, and `plan --json` carries the same thing as `"caveats"`, so a pipeline can tell "nothing to do" apart from "nothing I was willing to do".
- **Propose a deletion something still needs.** The Controller will not delete a network a WLAN is still on, a network with a DHCP reservation's address inside its subnet, or a zone one of your firewall policies still governs — so if this run leaves that WLAN, that reservation or that policy in place, the deletion is not in the plan at all, and the plan says which one kept it (see `docs/adr/0014-prune-skips-what-something-still-needs.md`):

  ```
  Plan: 1 to delete.

  ! network: the network "Lab" will not be deleted: this plan leaves the WLAN "Lab
    Wi-Fi" on it.
  ```

  The way to delete both is to put both at stake: a file with `networks:` and `wlans: []` deletes the WLAN and then the network under it, in that order.

  The Controller's own policies are the exception, and they have to be: it generates one for every pair of zones that holds a member, so a zone you create is governed by policies you never wrote and cannot put at stake. Those do not hold a zone back. Deleting such a zone on a real router returns `204` and the Controller reclaims the policies it generated for it — measured rather than assumed (see `docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md`).
- **Touch anything unifig does not manage.** WAN slots share a collection with your LANs; they are Settings, not Resources, so unifig updates them and never deletes one, whether or not your file names them. Nor does prune see a WLAN attached to something that isn't one of your LANs, or a port forward whose ports are a range — unifig has no way to write either one in config, so neither can be exported — and the two halves go together on purpose: what an adoption didn't write down is not something prune may delete. That covers what unifig couldn't describe and what it described and left out anyway: it words a policy your Controller generates every time it prints one in a caveat, and it still keeps that policy out of the file and out of prune's reach.
- **Delete more of a thing than the section names.** A DHCP reservation is two fields of the client record your Controller keeps, so pruning one gives the fixed address up and leaves that record — its name, its note, its group — exactly where it was. Forgetting the device is a bigger request than deleting a line of YAML, and unifig will not read the second as the first (see [DHCP reservations](#dhcp-reservations)).
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

A port forward whose ports are a range or a list gets the same treatment, and for the same reason — a port is half of what a forward is, so there is no partial way to write one down:

```
Left out 1 port forward, which unifig cannot describe: "Game server".
Each gives a port as a range or a list rather than as a single port, which
unifig does not model. It manages nothing about them, and `--prune` will not
delete them.
```

The firewall policies your Controller generates for itself are the one thing export leaves out that it *could* have written down. It computes one for every pair of zones rather than storing it, so there is no id to write to and no endpoint can edit it — an entry naming one would be a line no plan could ever act on. On a router migrated to the zone-based firewall that is every policy on the site, and the `firewall-policies:` key does not appear at all:

```
Left out 86 firewall policies the Controller generates rather than stores.
Each is computed from the pair of zones it governs, so it has no id to write to
and no endpoint can edit it. unifig manages nothing about them, and `--prune`
will not delete them.
To override one, write a policy of your own on the same pair under a name of
your own — the Controller's sits at the lowest precedence there is.
```

The name has to be yours. A policy is matched by its name *together with* the pair of zones it governs, so an entry keeping the Controller's name on the Controller's pair is that policy, and unifig matches it rather than creating one. Your own name is a key of your own, which is the create the way out promises. The return rules the Controller generates beside your allow policies are left out too, and are not in that count — one of those follows the policy it belongs to, which is already in the file.

A file you already have that names them all keeps working exactly as it did, and gets no warning: plan is silent when the file and the Controller agree, and a notice firing once per policy on a file that is working is how you learn to read past the ones that matter. Re-exporting is how you get the shorter file.

A WAN slot that connects in a way unifig does not model gets the other half of the same promise: the slot is in the file so you can see it exists, with nothing under it and a notice saying why.

```
Wrote 1 WAN slot with nothing but the slot: "WAN2".
Each connects in a way unifig does not model — static addressing, for example —
so there is nothing for the config to say about it. unifig will match the slot
and change nothing about how it connects.
```

And a Controller old enough to predate encrypted DNS is described by a file with no `encrypted-dns` section, which export says rather than leaving you to wonder whether it forgot:

```
Wrote no `encrypted-dns:` section: this Controller has no Encrypted DNS setting to describe.
```

A firmware whose encrypted DNS is in a mode unifig doesn't model gets the same treatment one level down — the section is written, the `state` is left out rather than written as something `validate` would reject, and export says which mode that was.

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
      "kind": "encrypted-dns",
      "name": "",
      "fields": [
        { "name": "state", "from": "off", "to": "custom" },
        { "name": "server \"Quad9\"", "from": null, "to": null, "secret": true }
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

`kind` is the managed type — `network`, `wlan`, `zone`, `firewall-policy`, `dhcp-reservation`, `port-forward`, `encrypted-dns`, `wan` — covering both the Resources unifig creates and deletes and the Settings it only updates. `name` is how unifig matched the change, and is `""` for a singleton Setting, which has nothing to match on. `risk` is present only on a change that needs individual confirmation. A field carries `notes` when the change has consequences your config does not state — the same sentences the prose plan prints under it, as a list, because one field can have several.

Changes are listed in the order apply will run them, so a consumer reading the array is reading the sequence.

## Development

`make` is the task entrypoint — run it with no arguments to list the targets. CI runs these same targets, so local and CI cannot drift.

```sh
make check   # fmt-check + lint + build + test; needs no Docker
make fmt     # format with gofumpt
make e2e     # the dockerized suite, against the newest Controller in the matrix
make matrix  # the same suite against every Controller version, then regenerate the table
make run ARGS="export"   # run against a live Controller, credentials from the environment
make record-udr          # re-record e2e/testdata/udr from a real UDR, read-only and scrubbed
```

Requirements: Go for `make check`, plus Docker for `make e2e`. `make` installs its own pinned golangci-lint (which carries gofumpt) into `bin/`; the pin lives in the Makefile.

Tests drive the whole tool at the process boundary against a real dockerized Controller (see `docs/adr/0003-apikey-auth-os-gate.md` for the rig design). That suite is `make e2e`; `make test` runs everything else, skipping it. There is no version pin in the rig: which Controller versions exist lives in `compatibility.yaml`, `make e2e` boots the newest of them, and CI fans out over all of them. The Controller image needs a database beside it, which the rig starts and throws away with the suite (`docs/adr/0016-the-matrix-needs-an-image-that-is-still-published.md`).

The Settings are what that container cannot stand in for — with no gateway it has no WAN entries at all, and no container can be trusted to say what a Setting looks like on the firmware you actually run — so those tests run against recorded Controller responses served at the same base URL, through the same API-key header, to the same real binary (`e2e/replay_test.go`). The recordings live in `e2e/testdata/udr/`.

The zone-based firewall is there for a blunter version of the same reason: the container does not have one. Its zone and policy collections are empty on every site including a freshly created one, and a zone create is refused outright — the feature belongs to a gateway, and no gateway is ever adopted (`docs/adr/0013-the-firewall-cannot-be-tested-against-a-container.md`). So Zones and Policies are tested against the recording too.

Everything in `e2e/testdata/udr/` came off a real UDR running Network 10.5.67, the firewall included: six built-in zones and eighty-three predefined policies, recorded minutes after that site was migrated to the zone-based firewall. A migrated router is the requirement rather than just a UDR — before the migration those endpoints answer `200 []` and `described-features` lists `ZONE_BASED_FIREWALL_MIGRATION` instead — so that is worth knowing before arranging access to hardware. `make record-udr` is how it is re-recorded: read-only against the Controller, scrubbed by a program with tests rather than by a filter in prose (`tools/record-udr/`), and stopping to make you read the diff.

The firewall fixtures were hand-written until that recording, and replacing them found two bugs no test in this repository could have: `--prune` proposing to delete every built-in zone, because unifig read a network's undeletable marker on a zone and no zone carries it (#23), and every migrated site refused as ambiguous, because a Controller reuses policy names across zone pairs (#24). Each fixture had been written in the same change as the code that read it, so the tests asserted the guess against itself and passed. That is the argument for recording from hardware, stated as a result. `e2e/testdata/udr/README.md` records what these files hold and where they came from. What a recording could not answer is what unifig sends when it *creates* a policy, since these files hold only the Controller's own; that one needed a write to a real site rather than a read, and got one on 18 August 2026. It round-trips: every field `newFirewallPolicy` sends comes back unaltered, and the Controller adds ten defaults of its own without rewriting anything it was given (`docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md`).

`docs/adr/0008-wan-slots-replay-recorded-responses.md` explains the design and what the hardware confirmed; `docs/adr/0011-a-recording-carries-only-the-uplinks.md` explains how much of a recording has to be real; `docs/adr/0012-encrypted-dns-is-a-singleton-setting.md` covers the second Setting.

Validate's tests are the exception, and deliberately so: it is offline by design, and requiring Docker to prove no Controller is needed would be an odd way to demonstrate it. They sit at the highest Docker-free seam instead — `cli.Run`, driven from an external test package in `internal/cli/` so they cannot reach past it.

`make matrix` is the compatibility promise, made mechanically: it runs the whole suite against every version in `compatibility.yaml` and regenerates `docs/COMPATIBILITY.md` and `internal/compat/matrix.json` from what those runs did (`tools/compat`). CI does the same thing one version per job and fails if the committed table is not what its runs produce, so the published table cannot say something no run said. Adding a version is a line in `compatibility.yaml`; so is adding a row, and the generator refuses a configuration that names tests which are not there or leaves a test file out of the table entirely.

Rig knobs: `UNIFIG_TEST_CONTROLLER_IMAGE` overrides the Controller image the matrix would have chosen; `UNIFIG_TEST_CONTROLLER_URL` points the suite at an already-running demo-mode Controller for a faster inner loop.
