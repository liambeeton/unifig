# Prune skips a deletion something still needs

`--prune` proposed deletions the Controller refuses. A file with `networks:` and
no `wlans:` key planned the deletion of a network a live WLAN was still on; a
file with `zones:` and no `firewall-policies:` key planned the deletion of a zone
a live policy still governed. Both are the two halves of ADR-0006 meeting each
other: the section the file has is at stake, the section it leaves out is out of
prune's reach, and deletions are ordered referencing-thing-first only among the
things actually being deleted — so a referencing thing that was never planned has
nothing in the plan to order.

The dockerized Controller answers such a delete with `400
api.err.ResourceReferredBy`, and apply stops there. Nothing is destroyed and
nothing is corrupted: the report says exactly what did and did not happen, and
re-running is safe (ADR-0001). What was wrong is that **the plan said something
would happen that could not happen**, and the operator approved it.

ADR-0005 already refused this shape from the other direction. "Attempt the delete
and treat the Controller's refusal as the exemption" was rejected there because
"the plan would advertise a deletion that cannot happen, and an apply would fail
partway through rather than never proposing it. A plan has to be a statement
about what will happen." That sentence decides this issue too. So: under prune,
unifig reads the referencing collection even where the file has no section for it,
leaves the deletion out of the plan, and says so.

## Which references count

A Resource whose config cannot be written without naming another one holds that
other one back. There are two, and the schema is what says so: a WLAN's `network`
is required, and a firewall policy's `source` and `destination` are required. A
policy with a zone missing is not a policy, and a WLAN with no network is not a
WLAN, so neither can be left behind pointing at nothing.

A zone's membership is deliberately not one of them. `networks:` on a zone is a
list that may be absent or empty, unifig already owns it per member rather than
wholesale (ADR-0004), and a zone holding one fewer network is still a zone. If
membership counted, every LAN on a migrated router would be held back by the
built-in Internal zone and `--prune` would stop deleting networks entirely.

That test — required in order to exist, rather than merely pointing — turns out to
name exactly the references the plan's ordering already exists for (`kinds` in
`reconcile.go`: a WLAN after the network it joins, a policy after the zones it
governs, and the reverse for deletions). The two lists are the same list because
they are the same fact about the Controller. A managed type that arrives with a
required reference of its own inherits the rule but not the wiring: it needs a
read gated in `ComputePlan`, a function saying what its area is leaving behind,
and the word its Caveat reads best under. What it does not need is a new answer to
what prune may propose.

## Which referencers count

The ones this plan leaves in place, rather than the ones on the Controller now.
The distinction is the whole of what keeps `--prune` working: a WLAN this same
apply deletes is not one keeping its network alive, so a file with `networks:` and
`wlans: []` still prunes both, in that order, exactly as before. Naming the
referencing section is how an operator puts it at stake, and that has not changed
— it is ADR-0006's own remedy, arriving where it is needed.

"Leaves in place" has to be read all the way through, and the two pairs need
different amounts of work to do it. A WLAN a file **moves** to another network is
one the plan spares and one the old network cannot be held back by: the update is
applied before any deletion, so by the time the delete runs the WLAN is elsewhere.
Reading the live binding there produced a plan that contradicted itself two lines
apart — `~ wlan "Lab Wi-Fi" network: Lab -> Studio` above a caveat saying the
plan leaves that WLAN on `Lab` — so a spared WLAN is taken to be on the network
the config states for it, and only otherwise on the one the Controller has. The
policy side needs none of that, and the reason is its key: a policy's pair of
zones is part of what identifies it (ADR-0001, #24), so a policy that changes
ends is not the same policy moving but a deletion and a create — and the deletion
is already in the plan.

Everything else that survives counts, **the Controller's own objects included**. A
zone's predefined policies are the case that makes this a decision rather than a
detail: the Controller generates a policy per zone pair and marks it
`predefined`, prune may not delete one (ADR-0005), so it will still be governing
its zone after the apply — and the zone is therefore not proposed.

It would be convenient to read `predefined` as the Controller promising to remove
its own policy along with the zone. Nothing establishes that. It is a guess about
someone else's product, of precisely the shape that has already cost this project
two bugs in the same area (#23, #24), and no recording can answer it — answering
it needs a write to real migrated hardware (ADR-0013). The rule therefore stays
the one that can be read off the Controller rather than inferred about it:
**something still points at it, so unifig does not offer to delete it.** The cost
of being over-careful here is a prune that says what it declined; the cost of
being wrong the other way is the bug this ADR exists to close.

**Answered on 18 August 2026, in favour of the convenient reading.** A write
session against the migrated UDR created a custom zone, watched the Controller
generate eighteen predefined policies for its pairs, and then deleted the zone:
`DELETE` returned `204` and the Controller reclaimed all eighteen itself
(`docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md`).
The paragraph above was the right position to hold while nobody had looked — the
cost of being over-careful really was only a prune that says what it declined.
It is the wrong position now: the deletion this rule declines to propose is one
the Controller performs without complaint, so the hold-back should narrow to
operator-authored policies, which is the scope issue #22 asked for. That change
is issue #28's, and it is a decision rather than a correction this ADR can make
on its own.

## And it says so

Each declined deletion is a `Caveat` — not a change, because nothing happens, and
not an error, because the run is correct (ADR-0005). It names the Resource, every
referencer that held it, and which of them a policy is by its whole key, since
nineteen of a migrated router's policies are called "Allow All Traffic" (#24).

```
Plan: 1 to delete.

! network: the network "Lab" will not be deleted: this plan leaves the WLAN "Lab
  Wi-Fi" on it.
```

Skipping quietly would trade one dishonest plan for a smaller one. An operator who
asked for a prune and silently got part of one reads the plan as a site with
nothing left to prune, and finds out later — which is ADR-0005's generalisation
applied where it was written to apply: anywhere unifig declines part of what was
asked and the result still looks like success, the plan owes the operator a
sentence.

## Considered Options

- **Annotate the deletion with what still references it, and plan it anyway**
  (issue #22's option 2) — rejected: it is honest about the reason and still a
  plan that says something will happen that will not. It also puts the operator's
  approval on a change unifig knows will fail.
- **Leave the behaviour and say so in the docs** (option 3) — rejected.
  Stop-on-first-error plus a safe re-run is the recovery for what unifig could not
  foresee, not a licence to propose what it could. The same argument would excuse
  never reading the built-in marker either.
- **Keep a rule about which references the Controller enforces** — rejected above:
  it is a guess about Ubiquiti's internals, and this project has paid for two of
  those already.
- **Read the referencing collection whether or not prune was asked for** —
  rejected: without `--prune` no deletion can be proposed, so the answer changes
  nothing, and a plan should not pay for a request it cannot use. This is the same
  reasoning that reads zone ownership only under prune.
- **Refuse the run and tell the operator to add the section** — rejected: the file
  that reaches this most often is a networks-only config on a Controller that has
  WLANs, which is the ordinary adoption path rather than a mistake.

## Consequences

- Prune reads more than it changes, and `ComputePlan` is where that is visible: a
  referencing area is planned before the area it points at, so the second can ask
  the first what it is leaving behind. Nothing about the order an operator reads is
  decided there — `sortChanges` does that, and puts deletions the other way round
  — so the two orders can be reasoned about separately.
- **The refusal itself is witnessed only for networks and WLANs.** The pinned
  container answered `400 api.err.ResourceReferredBy` while this bug was being
  reproduced, and it is still the thing enforcing the guarantee: the networks test
  applies its plan for real, so it passes only because the deletion is not
  proposed — propose it again and the Controller refuses it and the test fails. For
  zones and policies nothing can refuse anything: the stand-in serves a recording,
  and teaching it to refuse would be writing a fixture that asserts a guess
  (ADR-0013). What the firewall suite states is unifig's promise about its own
  plan, which is the part this ADR is about.
- **On a migrated router, `--prune` declines every custom zone.** Not "may": one
  custom zone made the Controller generate eighteen predefined policies for its
  pairs, and each of them holds the zone back. The zone is
  still deletable in the Controller's UI, and the plan names the policy that kept
  it, so this is narrower rather than silent. This was written as the bullet to
  revisit first if hardware ever showed the Controller cleaning those up itself,
  with a recording rather than a fixture — hardware showed exactly that on
  18 August 2026, so it is now the bullet being revisited, in issue #28
  (`docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md`).
- **Reading is not matching**, and the duplicate refusals moved accordingly: the
  WLAN and policy reads no longer refuse a site over two of them unifig cannot
  tell apart, and the verbs that actually match them do it instead. A file with no
  `wlans:` key had no business failing over two WLANs sharing an SSID, since
  nothing in that run was ever going to have to choose between them. The networks
  and the zones keep their refusal in the read, deliberately: those reads produce a
  binding from a name to the Controller ID that references are stored as, so two of
  one name there leaves every reference ambiguous whatever the file manages.
  Nothing is bound to a WLAN or to a policy.
- **A read that fails is not "cannot tell", and does not narrow the plan.** Where
  the built-in marker cannot be established, prune skips the deletions and says so
  (ADR-0005), because the answer is a property of the objects and may be
  unavailable on a Controller that is otherwise fine. A collection read that fails
  is the Controller not answering, and a plan computed around that would be a plan
  built on a Controller unifig cannot see — so it is an error for the whole run, as
  it always has been for a file that manages the section.
- Deletions of same-named policies now come out in the order the Controller listed
  them rather than a map's, because prune walks the collection instead of the
  keyed index. `sortChanges` leaves ties as it found them, so that was the last
  place a plan could print different bytes for an unchanged Controller.
