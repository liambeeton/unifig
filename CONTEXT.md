# Unifig

Unifig declaratively manages the configuration of a UniFi Network application on a UniFi Dream Router from human-readable YAML files.

## Language

### Model

**Controller**:
The UniFi Network application instance (on the UDR) that owns all network configuration. Unifig's single source of live truth.
_Avoid_: gateway, console, router (those name the hardware)

**Resource**:
A Controller configuration object with a full create/update/delete lifecycle and many possible instances, e.g. a network or a WLAN. Matched to config by a per-type natural key, never by stored ID.
_Avoid_: object, entity

**Setting**:
A fixed-slot or singleton configuration area of the Controller that can only be updated — never created, deleted, or pruned. WAN slots and Encrypted DNS are Settings. A slot is matched by the Controller's own name for it; a singleton is matched by being the only one, so it has no name at all and a Plan line about one carries none.
_Avoid_: resource (for these)

**Kind**:
The managed type a change is about — `network`, `wlan`, `zone`, `firewall-policy`, `port-forward`, `encrypted-dns`, `wan` — covering Resources and Settings alike, because an operator reads and approves a change to either the same way. It is the word a Plan line leads with and the field `plan --json` carries.
_Avoid_: resource (a Setting is not one), type (says nothing about what is being typed)

**WAN slot**:
One of the Controller's internet uplinks, and the first Setting. Matched by the Controller's own name for the slot — `WAN`, `WAN2`, `WAN_LTE_FAILOVER` — rather than by the entry's name, which an operator can rename to their ISP without it ceasing to be the primary uplink. Every change to one is a Risky change.
_Avoid_: WAN network (it lives in the same collection as the networks but is not one), WAN interface, uplink port

**Encrypted DNS**:
The Controller's setting for resolving DNS over an encrypted transport, which UniFi's UI calls DNS Shield. The second Setting and the first singleton: a Controller has exactly one, so there is nothing to match it by. Its custom resolvers are one field of it rather than a collection of Resources, which is why stating them in the config states the whole list.
_Avoid_: DNS Shield (Ubiquiti's marketing name for it), DoH (names one of the transports a DNS Stamp can describe, not the setting)

**DNS Stamp**:
The single `sdns://` string that describes an encrypted resolver — its address, protocol and public key in one value. A Secret, because a stamp for a private endpoint identifies the account it belongs to. It is how the config states a custom resolver, since it is the only thing the Controller asks for.
_Avoid_: DNS URL, resolver string

**WLAN**:
A wireless network (SSID) the Controller broadcasts. A Resource, matched by name, and bound to the Network its clients join — the reference that makes cross-reference validation necessary.
_Avoid_: SSID (that names the broadcast identifier, not the configured object), wifi network

**Zone**:
A named group of networks in the Controller's zone-based firewall. A Resource, matched by name. Custom Zones have the full lifecycle; built-in Zones (Internal, External, …) are matchable and updatable but never prunable, on the Controller's own marker rather than a list of names (ADR-0005). Its membership is one field rather than a collection, so stating it states the whole list — and a member unifig cannot name, such as the WAN in the built-in External Zone, is left exactly where it is rather than removed.
_Avoid_: firewall group (that is a different Controller object), interface group

**Firewall Policy**:
A rule governing traffic between a pair of Zones. A Resource, and the one whose key is not its name: it is matched by its name **together with the pair of Zones it governs**, because the Controller ships its own policies one per pair and reuses names across them — a migrated router answers with nineteen called `Allow All Traffic`. Two policies may share a name; two may not share a name and both ends. Its config states one required field beyond that key, a verdict (`allow`, `block`, `reject`). The Zones it names need not be in the config file, because the interesting ones are built-in, so that reference is resolved against the Controller rather than offline.
_Avoid_: firewall rule (that names the pre-Zone object the Controller still has), ACL

**Port Forward**:
A rule sending traffic that arrives on a port of the internet side to an address and port inside the network. A Resource, matched by name, and the one whose target is not a reference: it names an address rather than a Network, so nothing about it is resolved against the rest of the config and nothing it points at is held back from a Prune. Its ports are single ports — a live forward stating a range or a list is one the config cannot describe, so Export leaves it out and Prune never touches it.
_Avoid_: NAT rule, DNAT (those name the mechanism rather than the object), firewall rule

**DHCP Reservation**:
A fixed-IP assignment for a client, projected from the Controller's per-client record rather than existing as a standalone object. Its natural key is the MAC address.

**Reconcile**:
Computing and applying the difference between the YAML config and the live Controller directly; no intermediate state file exists.
_Avoid_: sync (ambiguous about direction)

**Prune**:
Deleting live Resources of a managed type that are absent from the config. Never implicit — only on explicit request, and built-in undeletable objects are always exempt. So is a Resource that something the same plan leaves in place still requires — a network with a WLAN on it, a Zone a Firewall Policy governs — because the Controller refuses to delete one and a Plan is a statement about what will happen (ADR-0014).

**Risky change**:
A change that can sever internet or management connectivity (e.g. any WAN/PPPoE mutation). Always individually confirmed, never silently applied — and never hard-blocked. The test is whether recovery could need physical access: an Encrypted DNS change can break name resolution for a whole site and is still not Risky, because the Controller stays reachable and the fix is one field away (ADR-0012).

### Workflow

**Plan**:
The previewed set of changes a reconcile would make. Read-only; mutates nothing.
_Avoid_: diff, preview

**Apply**:
Executing a plan against the Controller.
_Avoid_: push, sync

**Validate**:
Offline schema and cross-reference checking of the config files; touches no API.

**Interpolation**:
Replacing `${ENV_VAR}` references in config values from the environment as the file loads. Values only, never keys; the result is always text; substituted values are never rescanned. This is how secrets stay out of the committed file.
_Avoid_: substitution, templating (the latter implies logic, of which there is none)

**Secret**:
A modelled field whose value unifig will not print — a WLAN passphrase, a PPPoE password, a DNS stamp. It is a property of the field, not of where the value came from: a passphrase typed straight into the YAML is redacted exactly like one Interpolation supplied. Secrets are read back from the Controller so they can be diffed, and redacted everywhere they could leave: Plan prose and JSON, validation messages, and Export.
_Avoid_: credential (too narrow), sensitive field

**Export**:
Generating YAML config from the live Controller state. The brownfield adoption path.
_Avoid_: import (direction is Controller → files)

### APIs

**Internal API**:
The undocumented Controller HTTP API that manages the config plane; the only API that can do Unifig's job.
_Avoid_: legacy API, private API, classic API

**Integration API**:
Ubiquiti's official, documented API. It does not cover the config plane, so Unifig does not use it.
