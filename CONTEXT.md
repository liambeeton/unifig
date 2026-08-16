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
A fixed-slot or singleton configuration area of the Controller that can only be updated — never created, deleted, or pruned. WAN slots and Encrypted DNS are Settings.
_Avoid_: resource (for these)

**WLAN**:
A wireless network (SSID) the Controller broadcasts. A Resource, matched by name, and bound to the Network its clients join — the reference that makes cross-reference validation necessary.
_Avoid_: SSID (that names the broadcast identifier, not the configured object), wifi network

**Zone**:
A named group of networks/interfaces in the Controller's zone-based firewall. Custom Zones are Resources; built-in Zones (Internal, External, …) are matchable but never prunable.

**Firewall Policy**:
A rule governing traffic between a pair of Zones. A Resource, matched by name.

**DHCP Reservation**:
A fixed-IP assignment for a client, projected from the Controller's per-client record rather than existing as a standalone object. Its natural key is the MAC address.

**Reconcile**:
Computing and applying the difference between the YAML config and the live Controller directly; no intermediate state file exists.
_Avoid_: sync (ambiguous about direction)

**Prune**:
Deleting live Resources of a managed type that are absent from the config. Never implicit — only on explicit request, and built-in undeletable objects are always exempt.

**Risky change**:
A change that can sever internet or management connectivity (e.g. any WAN/PPPoE mutation). Always individually confirmed, never silently applied — and never hard-blocked.

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
