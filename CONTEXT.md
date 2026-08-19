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
The managed type a change is about — `network`, `wlan`, `zone`, `firewall-policy`, `dhcp-reservation`, `port-forward`, `encrypted-dns`, `wan` — covering Resources and Settings alike, because an operator reads and approves a change to either the same way. It is the word a Plan line leads with and the field `plan --json` carries.
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
A named group of networks in the Controller's zone-based firewall. A Resource, matched by name. Custom Zones have the full lifecycle; built-in Zones (Internal, External, …) are matchable and updatable but never prunable, on the Controller's own marker rather than a list of names (ADR-0005). One built-in is not like the others: the **Gateway Zone** is where the Controller itself answers, so it carries the site's management path and a Firewall Policy blocking traffic to it is the one firewall change that is Risky. Which Zone that is comes from the Controller's own key for it rather than from its name, for the reason the built-in marker does. Its membership is one field rather than a collection, so stating it states the whole list — and a member unifig cannot name, such as the WAN in the built-in External Zone, is left exactly where it is rather than removed.
Membership is **exclusive**, and the Controller is the one enforcing it: a Network is in exactly one Zone, always, so putting it in a second takes it out of the first, and taking it out of one moves it to the **Internal Zone** rather than to none. That is the second built-in that is not like the others — where the Gateway Zone is where the Controller answers, the Internal Zone is where it puts a Network nothing else holds — and it is found by the Controller's own key for the same reason. Exclusivity is a property of the Network as much as of the Zone, which is why a Plan about one Zone's membership names the other Zone too, and why a config file may not put one Network in two Zones (ADR-0020).
_Avoid_: firewall group (that is a different Controller object), interface group

**Firewall Policy**:
A rule governing traffic between a pair of Zones. A Resource, and the one whose key is not its name: it is matched by its name **together with the pair of Zones it governs**, because the Controller ships its own policies one per pair and reuses names across them — a migrated router answers with nineteen called `Allow All Traffic`. Two policies may share a name; two may not share a name and both ends. Its config states one required field beyond that key, a verdict (`allow`, `block`, `reject`). The Zones it names need not be in the config file, because the interesting ones are built-in, so that reference is resolved against the Controller rather than offline.
_Avoid_: firewall rule (that names the pre-Zone object the Controller still has), ACL

**Port Forward**:
A rule sending traffic that arrives on a port of the internet side to an address and port inside the network. A Resource, matched by name, and the one whose target is not a reference: it names an address rather than a Network, so nothing about it is resolved against the rest of the config and nothing it points at is held back from a Prune. Its ports are single ports — a live forward stating a range or a list is one the config cannot describe, so Export leaves it out and Prune never touches it.
_Avoid_: NAT rule, DNAT (those name the mechanism rather than the object), firewall rule

**DHCP Reservation**:
A fixed-IP assignment for a client, projected from the Controller's per-client record rather than existing as a standalone object. Its natural key is the MAC address, and it is the only key unifig folds case on, because the Controller lower-cases every MAC it stores. The record it is half of belongs to whoever set it in the UI — a name, a note, a user group — so unifig writes the address and nothing else, and giving a reservation up under Prune leaves that record exactly where it is rather than forgetting the device (ADR-0015). It names no network: the Controller decides which network an address belongs to by whose subnet contains it, which is also why a network with an address reserved inside it is one Prune holds back.
_Avoid_: static lease, fixed IP (that names the address rather than the assignment), client (that names the record a reservation is half of)

**Reconcile**:
Computing and applying the difference between the YAML config and the live Controller directly; no intermediate state file exists.
_Avoid_: sync (ambiguous about direction)

**Prune**:
Deleting live Resources of a managed type that are absent from the config. Never implicit — only on explicit request, and built-in undeletable objects are always exempt. So is a Resource that something the same plan leaves in place still requires — a network with a WLAN on it, a Zone one of the operator's own Firewall Policies governs, a network with a DHCP Reservation's address inside its subnet — because the Controller refuses to delete one and a Plan is a statement about what will happen (ADR-0014). A Policy the Controller generated for a pair of Zones is not one of those: it deletes such a Policy along with the Zone, measured on hardware rather than assumed (ADR-0019). What it deletes is the Resource rather than whatever the Resource is part of: giving up a Reservation leaves the client record behind.

**Risky change**:
A change whose recovery could need physical access. Always individually confirmed, never silently applied — and never hard-blocked. Two kinds of change qualify: any WAN/PPPoE mutation, and a Firewall Policy that would newly block traffic to the Gateway Zone, where the Controller answers (ADR-0018). Losing the site's internet is the usual consequence of one and never the test on its own — an Encrypted DNS change can break name resolution for a whole site, and a Firewall Policy can block the internet outright, and neither is Risky, because the Controller stays reachable and the fix is one field away (ADR-0012).

### Workflow

**Plan**:
The previewed set of changes a reconcile would make. Read-only; mutates nothing.
_Avoid_: diff, preview

**Apply**:
Executing a plan against the Controller.
_Avoid_: push, sync

**Backup**:
A copy of the Controller's own configuration, written by the Controller onto itself on request and left there. Apply can ask for one before it changes anything, and checks the Controller serves it back before the first change; unifig never downloads one and never restores one, because rollback is out of scope. There is one slot per Controller version rather than a file per backup, so a new one replaces the last — the automatic backups a console keeps of its own accord are a separate collection unifig does not touch (ADR-0017).
_Avoid_: snapshot (nothing is copied to where unifig runs), restore point (unifig cannot restore one), state (there is no state file, ADR-0001)

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

**Compatibility matrix**:
The Controller versions CI runs the whole process-level suite against, and the table generated from what those runs did (`docs/COMPATIBILITY.md`). It is evidence rather than a promise: a version is in it because a run passed, and the areas no container can answer for carry the version of the recording instead, since a recording is one router (ADR-0013, ADR-0016). A Controller running something the matrix does not carry is **untested**, which earns a warning on stderr and never a refusal — untested is not broken, and unifig has no evidence either way.
An **area** is one row of it — a body of behaviour with a test file behind it, such as Networks or Zones and Firewall Policies — and its **evidence** is which kind of Controller answered those tests, `container` or `recording`. Both are read out of the suite rather than declared, so a row cannot claim a kind of Controller its tests never talked to.
_Avoid_: supported versions (the table says what was tested, which is a narrower claim), certified

### APIs

**Internal API**:
The undocumented Controller HTTP API that manages the config plane; the only API that can do Unifig's job. What it answers a read with is not what it will take back on a write: an object comes back carrying the Controller's own read-only markers, and writing that object whole means writing it without them. That is a rule unifig keeps on every object it writes whole, rather than a claim about every endpoint — the Zone is where a write endpoint was measured refusing to be told about them, and the Firewall Policy is where the same rule is kept without one (ADR-0019).
What a write does with a field the body leaves out is **per endpoint**, and three of them have been measured. The v2 Firewall Policy endpoint **replaces**: a field left out is a field the operator loses, so writing that object whole means writing back what the Controller sent rather than what a library could read out of it — the read shape a struct discards is not a detail of deserialisation, it is the list of fields an update would delete (ADR-0021). The v1 endpoints — networks, WAN slots, WLANs, port forwards, client records — **merge**: a field left out is left alone, so an update sends the fields the config states — and, where the endpoint refuses a body without it, whatever it demands alongside — and the Controller puts the object back (ADR-0023). The v2 Zone endpoint **merges** as well, and shares no reasoning with either: it takes three fields and refuses every other field it sends on a read, so a body carrying the Controller's own back is a 400 rather than a preservation, and a field left out is left alone because leaving it out is the only thing a client may do with it (ADR-0024).

So the version in the path predicts nothing — the two v2 collections answer oppositely — and neither does the verb or the Resource. **Which endpoint** is the whole of the question, a new one is a fourth question rather than a case of the three, and every one of these answers cost a session with a real router to get. What all three share is the direction of the danger: a field on the wire is a field stored, whichever way it got there, so a Go zero value is as much an unrequested change as a dropped key.
_Avoid_: legacy API, private API, classic API

**Integration API**:
Ubiquiti's official, documented API. It does not cover the config plane, so Unifig does not use it.
