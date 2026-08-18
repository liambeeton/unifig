# A zone refuses unifig's payload, not the operator's change

Four questions had been waiting on the same thing: a **write** to a real migrated
UDR. A recording says what the Controller holds; none of them could be answered
by reading, and ADR-0014 had already refused to settle one of them by teaching
the replay stand-in to refuse, on the grounds that this would be "writing a
fixture that asserts a guess". Issue #30 ran them as one session on 18 August
2026, against the same UDR on Network 10.5.67 that ADR-0007, ADR-0008, ADR-0012
and ADR-0013 were closed against, migrated to the zone-based firewall the day
before. Every request and response was captured, refusals included, and scrubbed
on the conventions in `tools/record-udr/scrub.go`.

The session's headline is that the question everyone had been asking was the
wrong one. `attr_no_edit` had been read as the Controller reserving three zones
against modification, and the open question as whether unifig should honour it.
The Controller reserves nothing. unifig cannot write those zones because of what
unifig puts in the request.

## What the marker does not do

The site's six built-in zones carry `attr_no_edit: true` on exactly three —
`External`, `Vpn` and `Gateway` — as `e2e/testdata/udr/firewallzone.json` has
recorded since #21. Two controls established that the request shape works at
all: `Dmz`, unmarked and empty, and `Hotspot`, unmarked and already holding a
network, both accepted a membership PUT, and a read-back confirmed the member
was really there rather than a `200` over a discarded change.

`Vpn` and `External` both refused, with the same answer:

```
400: JSON parse error: Unrecognized field "attr_no_edit"
     (class com.ubnt.g.c.t.AWSXjrFfvsFZsv), not marked as ignorable
```

That is not a Controller declining to edit a reserved zone. It is a Controller
failing to parse the body, because the body carries a field its write DTO has
never heard of. The field comes from `go-unifi` v2.3.0, whose `FirewallZone` is

```go
NoEdit bool `json:"attr_no_edit,omitempty"`
```

and `omitempty` is doing all of the work: it drops the field when the value is
`false` and sends it when the value is `true`. So unifig's payload is well-formed
for exactly the zones whose marker is false and malformed for exactly the zones
whose marker is true. The marker correlates perfectly with the failure without
causing any of it, which is why reading the correlation off two runs would have
produced a confident and completely wrong rule. The same applies to
`cloud_template`, which the GET returns and the PUT also refuses: the read shape
of a zone is not its write shape, and nothing in the type system says so.

What settles it is a PUT built by hand carrying only `_id`, `name` and
`network_ids` — the shape unifig already sends successfully for an unmarked
zone. Against `Vpn`, `attr_no_edit: true`, it returned **200**, and an
independent GET confirmed the probe network was really in the zone. The
remote-user-VPN network the zone also held, which no config can name, was
preserved beside it — which is the behaviour
`TestStatingAZonesNetworksLeavesAMemberUnifigCannotNameAlone` asserts, confirmed
against hardware for the first time rather than against a stand-in that stores
whatever it is given.

So a zone marked `attr_no_edit` accepts a membership change. `README.md:138`'s
promise — you can name a built-in zone and manage what is in it — is true of the
Controller and false of unifig today, and the gap is a serialisation bug rather
than a policy question. `e2e/testdata/udr/README.md:71` dismissed the field as
meaning "editability, not ownership"; that reached the right shelf for the wrong
reason, since what it means is editability in the UI and nothing at the API.

## What the Controller deletes without complaint

ADR-0014 holds a zone back from `--prune` while any policy still governs it, and
conceded that for a **predefined** policy nothing established the Controller
would refuse the deletion at all — it is "at least as likely that it cleans them
up with the zone". That was the guess, and it is now measured.

Creating one custom zone made the Controller generate **eighteen** predefined
policies for its pairs, taking the site from 86 to 104. `plan --prune` then
declined to propose deleting the zone, naming all eighteen as the reason. A
`DELETE` of that zone returned **204**, and the policy count returned to exactly
86 — the Controller reclaimed every one of the eighteen itself.

So both halves of the concession resolve against the guard. The generation is
real, and the refusal is not: unifig is declining a deletion nobody has ever seen
refused, and now that somebody has looked, the Controller performs it without
complaint. The consequence in the field is that `--prune` can never remove a
custom zone on a migrated router — which is every router unifig targets — for a
reason that does not exist.

Two mechanisms were ruled out on the way, and are recorded because each was a
plausible story that would have been wrong in the ADR. An empty custom zone is
**not** reaped: one survived fifteen polls over five minutes, a zone write
elsewhere, and being emptied implicitly by another zone claiming its network.
A custom zone that appeared to vanish during the session had in fact been
deleted by a `DELETE` that succeeded, with the `404` that raised the question
coming from the same command being run a second time.

## A network lives in exactly one zone

Nothing in this repository said so, and the session found it by accident. Adding
the probe network to `Hotspot` removed it from `Dmz`, where the previous step had
put it. unifig planned one PUT, sent one PUT, and reported `1 to update`; two
zones changed. Removing a network from a zone does not leave it unzoned either —
the Controller reassigns it to `Internal`.

This matters beyond the curiosity, because ADR-0014's own standard is that a plan
is a statement about what will happen. A plan that names one zone while two
zones change does not meet it, and the operator's only warning is the diff they
read afterwards. Membership is a property of the network as much as of the zone,
and unifig models it only on the zone.

Policy generation tracks membership rather than mere existence, on the same
evidence: `Dmz` gaining its first member generated six policies of its own
(`Dmz`→`Gateway` infrastructure rules and `Hotspot`→`Dmz`), and losing it
reclaimed them. Nothing leaks; the count returned to baseline with identical ids.

## What a created policy comes back holding

The last open question from ADR-0013 and `README.md:563`: a recording holds only
the Controller's own policies, so what unifig sends when it *creates* one had
never been read back.

It round-trips. Every field `newFirewallPolicy` sends came back unaltered —
`enabled`, `protocol: all`, `ip_version: BOTH`, `connection_state_type: ALL`, the
`{ALWAYS, all-day}` schedule the Controller rejects a policy without, and both
`ANY`/`ANY` endpoints — with `action` mapped to `ALLOW` and both `zone_id`s
resolved to the pair named in the config. The Controller added ten fields unifig
does not model, all of them defaults it chose rather than corrections of anything
sent: `index: 10000`, `logging: false`, `predefined: false`, `icmp_typename` and
`icmp_v6_typename` of `ANY`, an empty `connection_states`, and four booleans
false. Nothing unifig sent was rejected, defaulted over, or silently rewritten.

While the policy existed, `plan --prune` proposed deleting it and left all 86
predefined policies alone — ADR-0005's rule that the exemption is read off the
Controller's marker rather than a list unifig keeps, confirmed on hardware for
policies as #21 confirmed it for zones.

## Considered Options

- **Gate `updateZone` on `attr_no_edit`**, as issue #27 proposed. Rejected on the
  evidence: it would decline an operation the Controller allows, make
  `README.md:138` false by choice rather than by accident, and encode a rule
  whose entire support is a correlation this session dissolved. The bug it would
  have hidden — a read-only field echoed into a write — would have survived to be
  found again somewhere else.
- **Keep ADR-0014's hold-back as it stands.** Defensible when nothing had seen a
  deletion attempt; not defensible now that one has returned `204`. Left in
  place, prune stays useless for custom zones on every router unifig targets.
- **Teach the replay stand-in to refuse marked zones**, which ADR-0014 refused as
  asserting a guess. The objection dissolves for a different reason than expected:
  the refusal is reproducible, but it belongs to unifig's payload rather than the
  Controller's rules, so the thing to cover is the request unifig builds.

## Consequences

- unifig must stop sending fields the write endpoint does not accept —
  `attr_no_edit` and `cloud_template` at minimum. Because `omitempty` hides the
  defect on exactly the zones most sites can test against, the cover for it has
  to be a request-shape assertion rather than a round-trip through a stand-in
  that stores whatever it is handed.
- ADR-0014's zone hold-back should narrow to operator-authored policies, which is
  what issue #22 originally specified; a policy the Controller marks `predefined`
  stops holding its zone back. That is a decision this ADR supports rather than
  takes. **Taken in issue #28**, on this evidence: ADR-0014 now carries the
  narrower rule, `zonesInUse` skips a predefined policy while
  `pruneFirewallPolicies` still spares it, and the e2e test that asserted the old
  behaviour asserts the new one.
- ADR-0013's closing paragraph and `README.md:563` no longer describe an open
  question, and `e2e/testdata/udr/README.md:71` can say what `attr_no_edit` does
  on evidence.
- `TestStatingAZonesNetworksLeavesAMemberUnifigCannotNameAlone` now has hardware
  behind it and carries a comment saying so, which was an acceptance criterion of
  #30 whatever the outcome turned out to be.
- Exclusive zone membership needs modelling before a plan can honestly claim what
  a membership change will do. Nothing here fixes it; it is filed rather than
  buried in an ADR nobody would search for it in. **Taken in issue #32**
  (`docs/adr/0020-a-network-lives-in-exactly-one-zone.md`): the plan names both
  sides of a move and says where a network taken out of a zone ends up, computed
  from the network's side of the model rather than the zone's. The section above
  is still where the measurement lives; the decision about what to do with it is
  there.
- The one thing this session could not answer is what any of it does on firmware
  other than 10.5.67. That is the standing limitation of a single household's
  router, and it is the reason `docs/COMPATIBILITY.md` exists.
