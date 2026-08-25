# A Firewall Policy states a narrowing, and a protocol that has no ports is how you clear one

An operator asked to keep a VLAN off the Controller's admin login page. unifig
could not express it, and the nearest thing it *could* express was a footgun —
which is the whole of why this ADR exists rather than a config example.

**A VLAN is a Network and a Firewall Policy governs a pair of Zones**, so the
unit is the zone and one-zone-per-VLAN is already how a file is written. That
part was fine. The destination was not. The login page is served by the
Controller, which answers in the Gateway zone — and so does everything else it
offers. Read off the live migrated UDR on 25 August 2026, each custom zone
reaches Gateway through exactly two Generated Policies:

```
Cyberdelia -> Allow mDNS          ALLOW  idx 30000        udp/5353
Cyberdelia -> Allow All Traffic   ALLOW  idx 2147483647   all
```

There is no generated `Allow DNS` and no `Allow DHCP` for a custom zone — only
the Hotspot zone ships those. DHCP leases, name resolution and time all ride
that catch-all, and every stored policy on that site sits at `index: 10000`,
above `Allow mDNS` and far above the catch-all. So `block Ellingson -> Gateway`
does not shut the login page. It shuts the VLAN.

`newFirewallPolicy` hardcoded `Protocol: "all"` and `PortMatchingType: "ANY"` on
both ends, and `fromLivePolicy` read back only the name, the verdict and the two
zones. **A Firewall Policy now states an optional narrowing**: a `protocol` from
six, and a list of destination `ports` that may be single ports or `low-high`
ranges.

## What the router answered

Four questions, all needing writes, measured on `Dmz -> Gateway` — a pair holding
no networks, so nothing rode either verdict, with the real destination on it
because ADR-0030 is what happens when a rule about a policy's ends is probed only
on a pair no real file uses. Every create used `block`, so no companion needed
cleaning up. The site came back field-for-field identical to a baseline of 135
policies.

- **A narrowing lands as sent.** `tcp` with `port_matching_type: SPECIFIC` and
  `port: "443,80"` was answered 201 and read back byte-identical on an
  independent GET.
- **Ranges are real.** `"8000-8010"` went through a PUT and read back unchanged,
  which is what the config's grammar rests on. go-unifi's generated pattern
  claimed it; nothing had asked the Controller.
- **`protocol: all` beside a SPECIFIC port is *accepted*.** 201, stored, read
  back holding both. The Controller does not enforce the rule unifig enforces.
- **Removing the `port` key clears the narrowing.** A PUT carrying
  `port_matching_type: ANY` with the key gone read back with no port at all,
  which is the mechanism the widening syntax below depends on.

An unasked-for fifth: the two creates in that session landed at `index: 10000`
and `10001`. ADR-0029 recorded "an unasked-for `index: 10000`" from a single
create, and this narrows it — two policies unifig creates on one pair have
creation-order precedence rather than undefined precedence. Nothing changes; it
is written down because ADR-0018 declined to model `index` and the next person to
wonder should find the reading rather than the gap.

## Omission is unmanaged, and a protocol that has no ports is the way back out

**This is the decision the feature turned on**, because two existing ADRs point
opposite ways at this exact field. ADR-0004: a modelled field the file omits is
unmanaged, not a request to empty it. ADR-0026: the Return Rule follows the
config rather than the policy's history, so that a policy cannot differ by
history invisibly to the plan.

It follows **ADR-0004**. A file that says nothing about ports has not asked for
every port; it has asked for nothing, so a policy narrowed in the Controller's UI
keeps its narrowing. The alternative would have every entry in every existing
file start claiming "any port" the day this shipped, and the next apply would
widen what an operator had narrowed by hand — a change to what files already
written mean, which is the one thing a config schema may not do quietly.

Which appears to leave no way to widen a policy again, and ADR-0004 is where the
answer is too. It has no removal syntax because *the schema gives no way to ask*
— there is no such thing as a network with no VLAN. Here there is: **a protocol
that has no ports is the statement that there are none.** Stating `all`, `icmp`
or `icmpv6` clears the port matching, because unifig will not write a port beside
a protocol that cannot carry one. That is ADR-0004's own "where a modelled
field's change strands an unmodelled one, unifig repairs the dependent field and
says so in the plan" — the DHCP-pool case, arriving a second time — rather than a
new rule or an invented sentinel value.

The rule generalises past `all` on purpose. `all` is what an operator will write
to widen, and it is not the only protocol with no ports; a rule that named `all`
alone would leave `icmp` beside a stale port as a shape unifig would write. One
predicate, three protocols, both directions: `validate` refuses ports beside one
of them offline, and the writer clears ports when one is stated.

**And this is the one place unifig is deliberately stricter than the
Controller**, which the probe is what established. `all` beside a specific port
is stored without complaint. It is refused here because it has no defined
meaning an operator could have intended, and because refusing it is what makes
"state `all` to widen" a rule rather than a coincidence. The cost of being
stricter is one config line an operator rewrites, and `validate` names both ways
out — give it a protocol that has ports, or drop the ports — because the two
readings of that mistake go opposite directions and a message naming one would
send half of them the wrong way.

## The destination end only, and six protocols

**Destination.** The Controller models ports on both ends and uses source ports
on its own policies — DHCP `68 -> 67`, mDNS `5353 -> 5353`. Every one of those is
a Generated Policy unifig will never write, and on client traffic a source port
is ephemeral. `source:` and `destination:` also stay plain zone-name strings
rather than becoming mappings, which would have broken every file in existence to
buy a symmetry nobody had asked for. `source-ports:` remains available if the
other end ever earns its way in.

**Six protocols** — `all`, `tcp`, `udp`, `tcp_udp`, `icmp`, `icmpv6` — out of the
Controller's thirty-seven, spelled exactly as the Controller spells them.
Thirty-seven is inheriting the Internal API's surface as unifig's public one,
which ADR-0004 rejected by name; the other thirty-one are transport and
tunnelling protocols nobody writes a home firewall rule about. Spelled rather
than translated because unlike a verdict — stored `ALLOW`, stated `allow` —
`tcp_udp` is `tcp_udp` on both sides, and a translation table with nothing to
translate is a table that can drift. A policy on one of the thirty-one reads back
stating no protocol, which is unmanaged, which is true.

**A list, with ranges**, against the Port Forward precedent. `CONTEXT.md` says a
forward the Controller holds as a range or a list "is one this file cannot
describe", and following that here would have made the login page take two
entries to say — 443 and 80 — which is a rule people write half of. The forward's
single ports were a modelling choice made when nothing needed more, and the way
to settle the inconsistency is to widen the forward onto this same grammar later:
one grammar adopted twice, not two grammars.

## Risky is unchanged, and the plan says what a wide block costs

`closesTheGateway` still asks only whether the destination is the gateway and
whether the verdict is becoming blocking. It does not ask about ports, so
`block tcp/443 -> Gateway` is Risky exactly as `block -> Gateway` is. Narrowing
it — firing only where the port set contains the management path — means unifig
keeping a list of Ubiquiti's management ports, which is the construct ADR-0005
and ADR-0018 have each already rejected and whose failure mode here is silence
when a port is added. ADR-0018 chose to over-warn for this reason: one extra `y`
against a UDR that needs physical access.

Note the tension deliberately: ADR-0018 offers "stop the IoT VLAN reaching the
internet" as its example of the change that gets no prompt, and this is its
neighbour and gets one.

What is new is a **note**, not a second risk. A blocking policy with no narrowing
pointed at the Gateway zone says out loud that it blocks every service the
Controller offers that zone — DHCP leases and DNS as well as the management UI.
It hangs off `protocol` or `action`, whichever the change has, and it fires only
where the change is the one bringing the block about: a create, a verdict turning
blocking, or a protocol widening back to `all`. A policy that was already a wide
gateway block and is having its return rule corrected has not newly cost anybody
their DHCP, and a note on that change is one an operator learns to read past
(ADR-0012). It hedges and stops where ADR-0018 stops — a statement about what the
policy says, never a verdict on what the rule set will do, because unifig models
neither `index` nor `enabled` and issue #1 puts lockout analysis out of scope.

## Two smaller calls worth finding rather than re-deciding

**The plan renders ports as one joined value, not an array.** The design said an
array of strings, on the reasoning that a heterogeneous array mixing integers and
range strings is hostile to consumers. Joining sidesteps that and matches
`nameList`, which is how every other list-valued field in a plan already renders
— so `plan --json` keeps every field value a scalar rather than growing one
exception. The separator is the plan's, `", "`; what goes on the wire is
`joinPorts`, with none.

**Export leaves out a `protocol: all` that narrows nothing.** It is what 126 of
135 live policies carry and it is the Controller's word for "no narrowing", so
writing it into every entry would be 126 lines saying nothing — which is how an
operator learns to skim a file. Leaving it out says the same thing in ADR-0004's
terms. This is export's and not `fromLivePolicy`'s, because the planner needs the
opposite: a file stating `protocol: all` against a policy already on `all` has to
plan as no change, and it can only do that by comparing against the live value.

## Considered Options

- **A purpose-built declaration** — `management-access: deny` on a zone, expanded
  by unifig into the right policy — rejected. It means unifig keeping a list of
  Ubiquiti's management ports, the construct rejected twice above, and its
  failure mode is the silent one: Ubiquiti adds a port and "deny" quietly stops
  denying. Modelling the narrowing puts the port list in the operator's file,
  where a wrong one is visible.
- **No model change; make the footgun loud and document the UI as the way** —
  rejected, but it is where the note above came from. It is honest and leaves the
  thing that was asked for unbuilt.
- **The config is the whole statement, as ADR-0026 made the Return Rule** —
  rejected above. It changes what files already written mean.
- **A `ports: any` sentinel to widen with** — rejected once a protocol that has
  no ports turned out to say it already. A sentinel would be a second way to
  state one thing, and the two could disagree.
- **Model both ends** — rejected as above; the source end has no author but the
  Controller.
- **Model all thirty-seven protocols** — rejected on ADR-0004's own grounds.
- **Narrow the Risky mark to policies whose ports include the management path** —
  rejected as above.

## Consequences

- **`validate` is stricter than the Controller in one place**, and it is the
  first time. Anything relying on unifig accepting whatever the Controller
  accepts is now wrong in that one spot, and the ADR says so rather than the
  behaviour being discovered.
- **A live policy narrowed by something unifig cannot describe exports without
  it** — a port group (`port_matching_type: OBJECT`), an inverted port match, an
  `APP`/`REGION`/`WEB` destination target, a source-side narrowing, or a protocol
  outside the six. The entry is truthful, because an entry stating no narrowing
  manages none. What it does not yet do is say so out loud, which is the promise
  every other export notice keeps. Filed as **issue #52**, and it must not stay
  open long: `all` beside a specific port is now a shape the Controller will hand
  back and `validate` will reject, so export could otherwise write a file it
  cannot read. Closed by **ADR-0032**, which keeps the entry and counts the
  omission.
- The update path's promise that "the schedule, **the port and address matching**,
  the logging switch and everything else an operator set in the UI survive" is
  now conditional, and the comment saying it has been amended. Port matching
  survives where the config states none and is overwritten where it states some.
- `applyNarrowing` and `storedPolicy.setNarrowing` are one rule in two halves,
  for the reason `overwriteManagedPolicy` and `storedPolicy.overwriteManaged`
  already were: a create writes onto unifig's defaults and an update onto the
  object the Controller sent. A seventh field unifig comes to own is a change to
  both.
- **Not modelled, and each declined for its own reason rather than by oversight**:
  `match_opposite_ports` (an inversion nobody has asked for), `port_matching_type:
  OBJECT` (a port group is a Controller object unifig does not manage),
  destination `matching_target` of `APP`/`APP_CATEGORY`/`REGION`/`WEB` (each a
  matching engine of its own), and `logging` (adjacent, and a change to what a
  policy means rather than to what it matches).
