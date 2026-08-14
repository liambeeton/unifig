# Built on the Internal API

As of UniFi Network 10.4.57, the official Integration API covers none of the config plane — no networks, WLANs, WAN, DNS, firewall, or port forwarding — so Unifig performs all resource operations through the undocumented Internal API (`/proxy/network/api/s/{site}/…`), accepting the risk that a Controller update can break it without notice.

## Considered Options

- **Integration API only** — rejected: it cannot manage a single Unifig resource type.
- **Hybrid** — rejected for v1: a second client stack buys nothing while the Integration API covers zero config resources.

## Consequences

- Version risk is mitigated by a Docker-based compatibility matrix in CI (support floor: Network 10.0) and warn-don't-refuse behavior on untested versions.
- Modern UniFi OS accepts API-key auth on Internal API endpoints (per the maintained go-unifi fork), so no password/cookie handling should be needed; verify against the UDR during implementation.
