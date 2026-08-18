# A network lives in exactly one zone

unifig planned one PUT, sent one PUT, reported `1 to update`, and two zones
changed. The write probe in issue #30 found it by accident on 18 August 2026,
against the migrated UDR on Network 10.5.67: adding the probe network to
`Hotspot` removed it from `Dmz`, where the previous step had put it, and nothing
in the plan or in the apply's report mentioned `Dmz` at all
(`docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md`).
Taking a network out of a zone does not leave it unzoned either — the Controller
reassigns it to the zone it keys `internal`.

ADR-0014's standard decides this: **a plan is a statement about what will
happen**. A plan naming one zone while two zones change is not one, and the
failure is quiet in the direction that matters. An operator moving a network into
a zone gets no warning that they are taking it out of another, and the only way
to find out is to diff the site afterwards — so on a site using zones for
isolation, the zone silently emptied is the one that was containing something.

## The models disagree, and only one of them can answer

unifig holds membership as a property of the **zone**: `config.Zone.Networks` in
the file, `zone.NetworkIDs` on the wire, `overwriteManagedZone` writing the
second from the first. The Controller holds it as a property of the **network**:
exactly one zone, always set. Both describe the same site, and only the second
can say what a single membership write does, because the thing that changes is
the network's zone rather than the zone's list.

That is why this was never "add a line to the plan output". The write stays as it
is — one PUT to one zone is what the Controller's API takes — and the *plan* is
computed with the network's side of the model in hand. `placement` (`zone.go`) is
that side: where each network is now, where this file puts it, and where the
Controller puts one that nothing claims. All three come from reads a plan already
does.

## What the plan says

Both sides of a move, on the zone whose change causes it:

```
  ~ zone "Hotspot"
      networks: "Guest" -> "Guest", "unifig-probe"
                the network "unifig-probe" is in the zone "Dmz" now, and a network
                belongs to exactly one zone: applying this takes it out
```

and where a network let go of actually ends up, which is never nowhere:

```
  ~ zone "Dmz"
      networks: "unifig-probe" -> (none)
                the network "unifig-probe" does not end up outside every zone: a
                network belongs to exactly one, so the Controller will move this
                one to "Internal"
```

Three decisions are inside those two sentences.

**The zone the Controller falls back to is read off its own key**, `zone_key:
internal`, beside the `gateway` key ADR-0018 already reads (`readZoneFacts`). The
alternative was the literal string `"Internal"`, which is the construct that had
prune proposing to delete every built-in zone (issue #23, ADR-0005). Where the
keys cannot be read, the sentence says the Controller will choose rather than
naming a zone unifig is guessing at. That made the read unconditional: it used to
happen only for a prune or a policy, and a plan that merely changes a membership
now needs it too.

**A network the same plan rehomes is not one the Controller has to find a zone
for.** Saying both would be a plan contradicting itself two lines apart, which is
the shape ADR-0014 refused once already over a WLAN being moved; so the losing
side names the gaining zone instead, and only says `Internal` when nothing in the
file claims the network.

**A file may not place one network in two zones.** It states two answers to a
question that has one, and which survived would come down to the order the writes
ran in — and the plan would say both, contradicting itself two lines apart, which
is the failure this whole ADR is about. `config.checkReferences` refuses it
offline, naming the zone that already has the network, which is the same refusal
a duplicate name gets one level in. The same check catches one zone listing a
network twice, which is a smaller thing with a concrete cost:
`overwriteManagedZone` appends one id per name, so the payload would carry the
same network twice.

## Can a network be stated as in no zone at all?

The question issue #32 asked to settle alongside the rest, and the answer is no —
there is nothing to express. `networks: []` on a zone already means "this zone
holds none", and it is the only sense in which a config can say a zone is empty.
What it cannot mean is "these networks are in no zone", because that is not a
state the Controller has: the network the list stops naming goes to the Internal
zone, and no field unifig could add would stop it.

The reason to say this rather than leave it open is that the config's silence is
already correct. A network no zone in the file names is unmanaged membership,
exactly as ADR-0004 has it — the Controller's answer is left alone rather than
overwritten with "none", and that is a different thing from stating a network
into no zone. Adding a way to say the second would be adding a way to state
something the plan would then have to decline.

## Considered Options

- **Say it as a Caveat rather than as field notes.** Rejected: a Caveat is an
  absence with a reason — something unifig was asked to do and declined — and
  this is the opposite, an extra thing that will certainly happen. `Field.Note`
  already means "a consequence of the change that the config does not state",
  which is exactly what this is. It became `Field.Notes`, because one membership
  list can displace one network, hand back another and hold a third unifig cannot
  name, and each is about a different network.
- **Teach the replay stand-in to evict, and assert the membership afterwards.**
  Rejected, and not on ADR-0014's usual grounds — the eviction is measured rather
  than guessed. The objection is that it would prove the wrong thing: the
  stand-in storing what it is handed is what makes "unifig's own model of the
  site" and "what the Controller would do" distinguishable at all, and a
  round-trip through a stand-in taught the Controller's rule would pass whether
  or not the plan ever mentioned the other zone. So the tests assert what the
  plan says.
- **Model membership on the network in the config file too** — `networks: [{name:
  IoT, zone: Untrusted}]`. Rejected for now: it matches the Controller exactly and
  would make the exclusivity unstateable rather than merely checked, but it moves
  a field between two sections of a file operators already have, and every
  built-in zone's membership would have to be expressible from the network's side
  before `export` could write it. The check does the same work for this issue.
- **Refuse the membership change and make the operator state both zones.**
  Rejected: the Controller performs the move happily, and unifig declining an
  operation the Controller allows is what ADR-0019 rejected for `attr_no_edit`.

## Consequences

- The recording shipped a site that cannot exist, and export is what found it.
  `tools/record-udr/scrub.go` folded every kept zone's LAN members onto the one
  committed LAN independently, so `Internal`, `Vpn` and `Hotspot` all held it —
  one network in three zones. Nothing in the firewall suite noticed; what noticed
  was `unifig export` writing that membership into a file and `unifig validate`
  refusing it two suites away. The fold is exclusive now: the first zone the
  router listed holding a LAN keeps it and the rest come back without one, which
  is arbitrary between them and is the most a recording carrying a single LAN can
  say. `e2e/testdata/udr/firewallzone.json` is updated to match, and
  `TestScrubLeavesEachNetworkInOneZone` states the rule.
- `replay.seedZone` takes a network out of whatever zone held it, for the same
  reason: a test seeding an impossible site is a test starting from somewhere its
  subject can never be. That is deliberately not the same as the stand-in's write
  path, which still stores exactly what unifig sent.
- **A deleted zone's members are named but not followed.** Where a network whose
  zone was deleted ends up has never been measured — the write probe deleted an
  empty custom zone and one whose member another zone had already claimed —
  and `Internal` there would be an inference from a different operation, which is
  the shape of guess that has cost this project three bugs already (#23, #24, and
  the `attr_no_edit` reading ADR-0019 dissolved). What the measurement does
  entail is that the network survives and is in *some* zone afterwards, because a
  network is in exactly one zone always; so a delete says that much and stops,
  and `- zone "X" / networks: "Lab"` no longer reads as a network going with the
  zone. Naming the zone is the bullet to revisit first if a hardware session ever
  answers it.
- **A Controller answering with one network in two zones is refused rather than
  reconciled.** Export writes the membership it reads, so such a site exports to a
  file `validate` will not accept, naming both zones. No such Controller has been
  seen — the state the Controller's own model forbids — and building a repair for
  one would be inventing behaviour for a site nobody has met. The message names
  the rule, which is what an operator needs to fix the file by hand.
- The limitation is ADR-0019's, unchanged: all of this was measured on one
  household's router running Network 10.5.67, which is why
  `docs/COMPATIBILITY.md` exists.
