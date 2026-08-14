# API-key auth is a UniFi OS gate; the test rig emulates it at the network level

Finding from the walking skeleton (issue #14): API-key handling on Internal API endpoints sits in the UniFi OS layer, not in the Network application. The real UDR reads and rejects a bogus `X-API-KEY` on `/proxy/network/...` with 401 before the request reaches the app (the positive case — a valid key accepted — still awaits an operator-created key; see Consequences), while the bare dockerized Network application (10.0.162) silently ignores the header, has no `/proxy/network` prefix, no key-creation UI or endpoint, and accepts only session cookies — its `apikey` service merely stores key documents and trusts identity headers forwarded by UniFi OS. Unifig therefore stays API-key-only (the UDR production path), and the process-level test rig fronts the dockerized Controller with a reverse proxy that emulates the OS gate: it answers the SDK's style probe, rejects requests whose `X-Api-Key` doesn't match the rig key, and forwards `/proxy/network/*` to the real Controller with a rig-held admin session.

## Considered Options

- **Add session (username/password) auth to unifig for tests** — rejected: the tool would exercise a different auth path and API style in tests than in production, and the spec forbids storing an admin password.
- **Skip live-Controller tests for anything auth-shaped** — rejected: the walking skeleton exists to prove the process-level rig.

## Consequences

- The spec's "Controller substituted at the network level, behind the same base URL" now has a concrete house pattern (`e2e/rig_test.go`); the WAN/Encrypted-DNS replay rig (issue #6) can follow it.
- The rig's proxy is UniFi OS make-believe only for **auth**; every config-plane response still comes from a real Network application.
- Verifying the positive case on the real UDR (valid key → 200) still needs an operator-created API key; only the negative half (key read and rejected at the OS layer, new-style API present) is verified so far.
