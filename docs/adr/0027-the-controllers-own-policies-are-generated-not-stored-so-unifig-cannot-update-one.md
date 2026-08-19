# The Controller's own policies are generated, not stored, so unifig cannot update one

`updateFirewallPolicy` could not land a change on any of the eighty-six Firewall
Policies a migrated UDR ships, and `internal/reconcile/policy.go` said the
opposite in as many words — ADR-0018 rested an argument on it:

> a predefined policy is matchable and **updatable** like any other — only prune
> exempts it (ADR-0005)

Matchable, yes. Updatable, no. `unifig.yaml` in this repository names nineteen
`Allow All Traffic` policies, all of them the Controller's own; changing the
verdict of one — the single edit the config models — planned cleanly and then
failed on apply.

## What was measured

Against the live migrated UDR on Network 10.5.67, 19 August 2026, as issue #41's
own probe. The site's baseline is 86 policies, all `predefined`, none custom, and
it was returned to that baseline byte for byte at the end.

**Every one of them is computed from the pair of zones rather than stored.** The
`_id` says so literally: 86 of 86 satisfy

    _id == source.zone_id + destination.zone_id + index

concatenated, giving 53 characters where the index has five digits and 58 where
it has ten. The one custom policy the probe created had an ordinary 24-character
document handle, `predefined: false`, and no `origin_id`. Every generated one
carries an `origin_id`, each a distinct real handle, and none of them is either
zone's id.

**The Controller resolves a document handle and nothing else.** Every PUT in this
probe carried the body the Controller is measured refusing — `create_allow_respond:
true` beside a non-`ALLOW` verdict (ADR-0022) — so that no request could write
anything whatever the id did:

| id | body | answer |
| --- | --- | --- |
| the throwaway's real handle | refused combination | **400** `FirewallPolicyCreateRespondTrafficPolicyNotAllowed` |
| an absent handle, `000…000` | the same body | **404** `api.err.FirewallPolicyNotFound` |
| `Block All Traffic`, composite `_id` | its own stored body, verbatim | **404** `api.err.FirewallPolicyNotFound` |

The first two rows are the calibration and they are the reading that makes the
third mean anything: **the id is resolved before the body is looked at**. A
resolvable id reaches the validator and is answered 400; an unresolvable one is
answered 404 without the body being read. So the third row is the id failing,
not the payload — and no PUT here could have written to one of the Controller's
own policies even if it had resolved.

GET agrees, across both length shapes and both `origin_type` values, including
`Allow All Traffic` from Internal to Gateway — the policy ADR-0018's Risky mark
was written for.

**No endpoint can edit one.** This was established rather than assumed, which is
the half of issue #41 that could have been left as a shrug:

- `GET|PUT /firewall-policies/{origin_id}` -> 404. Whatever `origin_id` points
  at, it is not a handle into this collection.
- The collection has no sub-resource routes at all. `/firewall-policies/batch`
  answers `FirewallPolicyNotFound` with `details._id = "batch"`, so every path
  segment is parsed as an id.
- `/firewall-rules` and `/firewall/policies` under v2 are Tomcat 404s — no such
  route.
- The legacy `rest/firewallrule` collection is **empty** on a migrated site, and
  `rest/firewallrule/{origin_id}` returns no entry.
- `origin_id` appears in none of `firewallrule`, `firewallgroup`, `networkconf`,
  `routing`, `portforward`, `wlanconf`, `usergroup`, `dynamicdns`, `setting`, or
  the zone collection.

The composite is the only id the payload carries and the Controller does not
resolve it. Anything that edited a shipped policy — the Controller's own UI
included — would need an id, and there is none to use.

## A Generated Policy is a new thing in the domain, and the `_id` is what names it

`CONTEXT.md` gains **Generated Policy**: a Firewall Policy the Controller
computes for a pair of Zones rather than storing, readable and matchable and
never writable. It is not a new kind of object so much as a fact about ones
already named — the Controller's own predefined policies are all of them, and so
is every Return Rule.

**The predicate reads the `_id`, not `predefined`.** The two agree on every
policy anyone has measured, and they are still different claims: one says whose
object it is, the other says whether there is anything to write to. Reading a
marker to decide what a write can do is the mistake issue #34 corrected once
already — `attr_no_edit` was taken for a statement about which zones may be
edited and marked nothing of the kind (ADR-0019). A firmware that started storing
its own policies would be followed by an `_id` test and refused forever by a
marker test. `pruneFirewallPolicies` still spares on `predefined`, because what
prune asks really is whose object it is (ADR-0005).

## The plan holds the change back and says so

A plan is a statement about what will happen (ADR-0014), so a change that cannot
be applied is not one to plan. `planFirewallPolicies` computes the difference as
before — the difference is real, and computing it is what decides whether there
is anything to say — and then, for a Generated Policy, emits a `Caveat` instead
of the `Change`.

- **A Caveat rather than an error.** The run is correct and the rest of the file
  still applies; refusing the whole plan would let one unwritable policy stop
  every other change in the file. That is the type's own reasoning and prune's
  hold-back is the same shape.
- **A Caveat rather than a silence.** An operator who edits the verdict of `Allow
  All Traffic` and is told "No changes. The Controller already matches the
  config" has been lied to about their own file. This is the first caveat about a
  change the operator explicitly asked for rather than one unifig proposed itself.
- **Only where something differs.** All nineteen `Allow All Traffic` policies are
  generated, so a caveat per generated policy would put nineteen lines under every
  firewall plan, which an operator reads past by the third run — the gate
  `unreadableGateway` already applies to itself.
- **It names the policy by its whole key**, because a migrated router ships
  nineteen of that name (ADR-0001, issue #24), and it ends with the way out: a
  policy of the operator's own on the same pair takes precedence, because the
  Controller's sits at `index: 2147483647`.

## ADR-0018's Risky mark is reconciled rather than removed

That ADR's leading example was the Controller's own `Allow All Traffic` from
Internal to Gateway, "a one-line edit away from being the rule that locks the
operator out", and it rejected "no firewall change is Risky" on exactly that
ground. The edit cannot be made, so the mark on it guarded a non-event — and a
confirmation for a non-event is how a prompt stops being read (ADR-0012). The
plan now holds that change back before the mark is ever reached.

**The mark keeps its subject through the ADR's other argument.** A policy the
operator *creates* over the Controller's own takes effect, because the
Controller's sits at the lowest precedence there is. So the lockout is still one
line of config; it is a `+` rather than a `~`. Nothing about the rule changes —
what changes is which shape of change can carry it.

## Considered Options

- **Refuse the whole plan, the way `noSuchZone` does.** Rejected. It is loud and
  impossible to miss, and one unwritable policy would stop every other change in
  the file from applying until the operator edited it out — a heavy answer to a
  file that is otherwise fine, and it makes `unifig.yaml` in this repository
  unplannable the moment anyone touches a verdict.
- **Plan the change and let apply fail.** Rejected: that is the status quo and it
  is what ADR-0014 exists to forbid.
- **Gate on `predefined`.** Rejected, above.
- **Re-create the policy under unifig's own id.** Rejected as a change of
  meaning: it would leave the Controller's own policy in place underneath and add
  a second one, which is the workaround the caveat suggests to the operator
  rather than something to do behind their back.

## The warning outlives the mark

Dropping the mark on a change nobody can apply is only half of the reconciliation,
and the other half is a trap this walked into first. The caveat ends by telling the
operator the way out — write your own policy on the same pair — and on the Internal
to Gateway pair that is *precisely* the create ADR-0018 marks Risky. A plan that
removed the confirmation and then recommended the dangerous thing, with the danger
left out, would be worse than the state it replaced.

So `unwritablePolicy` takes whether the change it is holding back would have closed
the path to the Gateway, and appends `gatewayRisk` — the same sentence the mark
prints — to the way out. On any other pair it says nothing extra, because a warning
on every caveat is a warning read past (ADR-0012).

## Consequences

- **The scrub must preserve an identifier's shape, not only anonymise its value.**
  `tools/record-udr/scrub.go` mapped every `_id` through a 24-character
  placeholder, so the committed recording held no composite id at all and the
  replay stand-in had never been handed one. Every policy in the suite was
  addressable and every update passed — ADR-0019's rule (a stand-in that accepts
  what hardware refuses is a fixture asserting the wrong guess) arriving through
  the **recording** rather than through the stand-in. A composite is now scrubbed
  component by component and put back the way it came, so a recorded policy's
  `_id` still equals its own scrubbed zone ids and index.
- **The stand-in answers 404 to an id that is not a document handle**, before it
  looks at the body, and fails the test when it has to — unifig must never send
  one, so a PUT arriving there is the hold-back having regressed. The code and
  message hang off the collection's `writeContract`, because they are the policy
  endpoint's own: the zone collection shares the code path and has never been
  asked, so it names none and the check does nothing for it.
- **A recording taken before this carries the wrong shape** and has to be
  re-recorded for the fixture to state the distinction. `make record-udr` is the
  way, and its diff gate is a human's — so the committed recording still holds 83
  plain ids as this is written, and `e2e/testdata/udr/README.md` says so rather
  than claiming otherwise. Until it runs, the distinction is exercised only by
  `seedGeneratedPolicy`, which is the honest position: the stand-in knows the
  rule, and the recording does not yet demonstrate it.
- The e2e tests seed their Generated Policy on the `Dmz -> Dmz` pair, which is
  the pair ADR-0022 and ADR-0026 probed on hardware for the same reason: the
  recording ships no `Allow All Traffic` there, and nothing rides it.
- **`origin_id` is confirmed to be reachable by nothing**, which retires the last
  of ADR-0021's worry about severing a policy's back-reference: only a generated
  policy has one, and no update can reach a generated policy.
