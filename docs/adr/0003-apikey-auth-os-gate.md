# API-key auth is a UniFi OS gate; the test rig emulates it at the network level

Verified during the walking skeleton (issue #14): API-key auth on Internal API endpoints is enforced by the UniFi OS layer, not by the Network application. The real UDR validates `X-API-KEY` on `/proxy/network/...` — a bogus key gets 401 before the request reaches the app, and an operator-created key gets 200 (`unifig export` returned the site's networks end-to-end) — while the bare dockerized Network application (10.0.162) silently ignores the header, has no `/proxy/network` prefix, no key-creation UI or endpoint, and accepts only session cookies — its `apikey` service merely stores key documents and trusts identity headers forwarded by UniFi OS. Unifig therefore stays API-key-only (the UDR production path), and the process-level test rig fronts the dockerized Controller with a reverse proxy that emulates the OS gate: it answers the SDK's style probe, rejects requests whose `X-Api-Key` doesn't match the rig key, and forwards `/proxy/network/*` to the real Controller with a rig-held admin session.

## Considered Options

- **Add session (username/password) auth to unifig for tests** — rejected: the tool would exercise a different auth path and API style in tests than in production, and the spec forbids storing an admin password.
- **Skip live-Controller tests for anything auth-shaped** — rejected: the walking skeleton exists to prove the process-level rig.

## Consequences

- The spec's "Controller substituted at the network level, behind the same base URL" now has a concrete house pattern (`e2e/rig_test.go`); the WAN/Encrypted-DNS replay rig (issue #6) can follow it.
- The rig's proxy is UniFi OS make-believe only for **auth**; every config-plane response still comes from a real Network application.
- Both halves are verified on the real UDR: a bogus key is read and rejected at the OS layer, and a valid operator-created key authenticates `unifig export` end-to-end (UniFi OS 5.1.19, Network 10.5.67, 2026-08-14). No password/session fallback is needed, confirming ADR-0002's expectation.
