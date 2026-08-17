# Secrets read back from the Internal API, so they diff like any other field

The spec left this open (issue #1): the Internal API was *expected* to return secret fields to admin credentials, making normal diffing work, with write-only diff semantics as the fallback if a field proved unreadable. Verified against the **dockerized** Controller (Network 10.0.162): a WLAN's `x_passphrase` comes back in the clear on `GET /api/s/<site>/rest/wlanconf`, byte-identical to what was written, on a plain list as well as on the create response. No fallback is needed, and none is built.

**The physical UDR agrees**, as of 17 August 2026, on Network 10.5.67. This paragraph used to record the gap as outstanding and name the one command that would close it — `unifig export` against the UDR, read-only. That command has now been run, and it redacted six secrets: four WLAN passphrases, the PPPoE password, and the Encrypted DNS stamp. Redaction only invents a variable for a value that is there, so all three kinds of secret unifig models come back populated from the real hardware, through the real binary, over the OS-level API-key gate ADR-0003 describes.

One distinction survives that, and it is worth stating rather than rounding off. Export proves the fields are **populated**; it cannot prove they are the **true** values, because it never prints them. A Controller that returned a constant mask would look identical here and would fail differently — not a permanently non-empty plan from a missing field, but a permanently non-empty plan from a value that never matches. Both are caught by the same one command, which is `unifig plan` against the UDR with those six variables exported: an empty plan is the proof, and nothing short of it distinguishes the two.

That single fact is what the whole secret story rests on. A passphrase unifig could write but not read would leave reconcile with no way to tell "already correct" from "needs changing", and both answers are wrong: propose the change every time and no plan is ever empty, or propose it never and a rotated passphrase in the file silently does nothing. Because the value reads back, a passphrase is an ordinary field in an ordinary diff — the same `changedFields` comparison as a subnet — and idempotence is asserted the same way as everywhere else, by re-planning after an apply and getting nothing (`TestApplyRotatesAPassphraseAndTheNextPlanIsEmpty`).

Readable does not mean printable. A secret is redacted at every point where it could leave unifig — plan prose, plan JSON, validation messages, export output — and that redaction is behaviour, not a formatting nicety, so it is tested as such at the process boundary.

Two neighbouring facts were pinned down at the same time, both against the Controller's own answers rather than the SDK's generated field spec:

- The passphrase bound is **8 to 64** characters, not the 8 to 255 the SDK's generated `validate` pattern claims. 65 is refused. The JSON Schema uses the real bound so validate catches it offline.
- The Controller's own field rule is **printable ASCII** (a passphrase with accented letters is refused). The schema carries that as a `pattern`, so validate catches it offline like everything else — but doing so meant fixing the reporter first. unifig's `pattern` message quotes the value it rejected, which is right for a subnet and catastrophic for a passphrase, so `internal/config/schema.go` keeps a small `secretFields` list and prints the expectation without the value for anything in it. Constraining a secret and never printing one are both achievable; it just takes the two of them to be decided in the same place.

## Considered Options

- **Assume write-only semantics and never diff a secret** — rejected: it is the strictly worse behaviour, and it was only ever the fallback for a limitation that does not exist.
- **Diff a hash of the secret instead of the value** — rejected: with the plaintext readable there is nothing to buy, and it would add a second representation to keep in step.
- **Leave the printable-ASCII rule to the Controller and out of the schema** — rejected, though it shipped first. It kept the reporter simple at the cost of the promise validate exists to make (issue #1, story 4: catch mistakes "without touching the router"), and it left a validation hole load-bearing, because the partial-apply test was using it as its trigger. The two places that now have to agree about which fields are secret is a real cost; a one-entry list next to the hints it sits beside is a smaller one than a passphrase reaching a terminal.

## Consequences

- Every later secret (WAN PPPoE credentials in issue #6, the Encrypted DNS stamp) starts from "it diffs normally" and only needs its own redaction, unless its own readback check says otherwise. Each area verifies its own fields; this ADR is about WLAN passphrases and the pattern they set.
- Confirmed on the dockerized Controller the CI matrix runs (10.0.162) and, for readability of all three secrets, on a physical UDR running 10.5.67. The compatibility matrix is what keeps it true across versions: a firmware that started withholding the field would turn every WLAN plan non-empty, which the idempotence tests catch immediately rather than subtly.
- Redaction is now the only thing standing between a secret and a terminal, so it is asserted directly rather than incidentally: `internal/export/redact_test.go` for the rewrite, and the e2e suite for every stream unifig writes to — plan prose, plan JSON, apply output, and a stopped apply's report.
