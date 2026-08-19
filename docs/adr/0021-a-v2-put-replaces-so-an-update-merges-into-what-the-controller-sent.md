# A v2 PUT replaces, so an update merges into what the Controller sent

`updateFirewallPolicy` read the live policy, changed unifig's own fields and put
the whole object back, with a comment beside it promising that "the schedule, the
port and address matching, the logging switch and everything else an operator set
in the Controller's UI survive". The object it put back was a
`unifi.FirewallZonePolicy`, so everything the Controller sent that `go-unifi`
v2.3.0 does not model had been dropped at unmarshal, long before the write.

Whether that cost anything rested on one thing nothing in this repository had
measured: **does a v2 PUT replace the object or merge into it?** If it merges,
unifig loses nothing and the comment is true by accident. If it replaces, every
update quietly resets the part of the policy unifig cannot name.

The replay stand-in answered "replace", and answered it as a fixture asserting a
guess — the thing ADR-0014 had already refused to do once, held to a lower
standard than `refusedByZoneWrite` beside it, whose comment says "Only what was
measured is here". The single piece of evidence that existed pointed the other
way: #30's hand-built PUT to `Vpn` carried `_id`, `name` and `network_ids` and
nothing else, returned 200, and left the zone intact.

## What was measured

Against the live migrated UDR on Network 10.5.67 (`UDR.mt7622.v5.1.19`), 18
August 2026, as issue #35's probe. A throwaway custom policy `unifig-probe-35`
(`Dmz` -> `Dmz`, which nothing rides) was created by `unifig apply`, then
narrowed **in the Controller's UI** to protocol `icmp`, IPv4 type
`ECHO_REQUEST` — a narrowing unifig's config cannot express and therefore cannot
have sent. Then `unifig apply` changed one managed field, `action`, allow ->
block, which is exactly the PUT in question. Reading the policy back:

```
icmp_typename:  "ECHO_REQUEST"  ->  "ANY"
```

**A v2 PUT replaces.** unifig changed the verdict and destroyed the operator's
ICMP narrowing in the same request: the policy went from matching echo requests
to matching every ICMP type. The plan did not mention it, the apply output did
not mention it, and the comment promised it could not happen. It is a live-path
defect rather than a hypothetical, and the silence is the worst part of it — an
operator gets no signal that a one-field edit reverted something they set by
hand.

Two findings widen it past the table issue #35 opened with:

- **`description: ""` vanished from the stored object.** go-unifi *does* model
  `description`, but `omitempty` elides an empty string, so it never reached the
  wire and the replace dropped the key. The loss is therefore not confined to
  fields the library has never heard of: **anything `omitempty` elides at its
  zero value goes too**, and any fix scoped to "fields go-unifi does not model"
  would still have been wrong.
- **`schedule.time_all_day: false` was written where the key had not existed.**
  The mirror image: a Go zero value becoming a stored value the operator never
  set. A field unifig invents is as much an unrequested change as one it drops.

**`index` is accepted back and kept.** unifig sent the live value (10000, what
the Controller assigns a custom policy; the predefined band is 30000-30008 and
2147483647) and it survived the PUT unchanged — no 400, no reordering. So
nothing needs to change there, and `batch-reorder` is not implicated in an
ordinary update. That was the second open question of #35 and it closes without
a code change.

**The zone endpoint is not settled by any of this**, and inferring it from here
would be wrong twice over. Step 0 of the probe read all six built-in zones still
carrying every field unifig's payload could not have contained — `cloud_template`,
`external_id`, `zone_key`, `default_zone`, `attr_no_edit` — including on the four
written during #30, which looks like a merge and contradicts what was measured on
policies. The likeliest explanation is that every survivor is a field the
Controller can regenerate for a **built-in** zone. Settling it wants a custom
zone, whose `external_id` cannot be regenerated, and that is a different endpoint
and a different session — **issue #38**, with the reading that would settle it.

## Considered Options

- **Change the comment and leave the code.** Cheap, and available: issue #35's
  acceptance allowed it. Rejected because it documents a defect rather than
  fixing one — the accepted answer to "does unifig preserve what the operator
  set" would stay **no**, on the one Resource whose whole point is that unifig
  models three of its fields and the operator owns the other thirty.
- **Preserve a curated list of fields.** Rejected by the `description` finding
  before it was written: a list scoped to the six fields the issue tabled would
  have missed a field go-unifi models, and the next firmware's seventh field
  would be missed the same way. A list is a guess about the Controller's shape,
  and this repository has one rule about those.
- **Merge into the object the Controller sent** — chosen. The update reads the
  policy as raw JSON, writes unifig's four managed values onto it and puts that
  back, so the body differs from what the Controller sent by exactly what the
  config says and nothing else. It makes ADR-0004's update rule — "reads the live
  Resource, overwrites only the modelled fields and puts the whole object back" —
  true on the wire rather than true of a struct.

## Consequences

- `updateFirewallPolicy` writes through `mergeIntoStoredPolicy`, and the read it
  merges into happens at the moment of writing rather than at the moment of
  planning. That fixes a second, smaller silence for free: a change the operator
  made in the UI while reading the plan is no longer reverted by approving it.
- `writablePolicy` is gone, and the marker rule is now kept by name over the
  whole `attr_*` family rather than by clearing the four fields go-unifi models
  and letting `omitempty` drop them. That is the rule ADR-0019 and both marker
  tests already stated; what changed is that merging into the Controller's own
  object would otherwise send back a marker the library has never heard of.
- **The policy write DTO takes all six fields back, and stores four of them
  nowhere.** This was the open question, filed as issue #37, and it was measured
  on the live migrated UDR on 19 August 2026 rather than reasoned about. A
  throwaway `Dmz` -> `Dmz` policy was PUT back carrying `origin_id`,
  `origin_type`, `icmp_typename`, `icmp_v6_typename`, `hits` and `last_hit`
  together, and the answer was **200** — no 400, no field named, nothing like
  #27. Read back, four of the six were simply absent from the stored object:
  `origin_id`, `origin_type`, `hits` and `last_hit` were sent, accepted and
  dropped. They are the Controller's own to write, and a body carrying them is
  neither refused nor believed. `icmp_typename` and `icmp_v6_typename` are
  stored, which is the point — those are the operator's narrowing, and carrying
  them is what this ADR exists for.

  So the loud failure this ADR accepted the risk of does not happen on these six,
  and nothing has to be withheld. The zone endpoint remains the reason that was
  worth measuring rather than assuming: two v2 collections, two DTOs, and only
  one of them refuses what it has not heard of.

  That reading left one thing open — whether a *generated* policy's own
  `origin_id` survives an update, as against the DTO merely not taking the field
  off a body — and going after it dissolved the question instead of answering
  it. `origin_id` appears only on policies the Controller generated, and those
  are exactly the policies unifig **cannot address**: their `_id` is a composite
  of `source_zone_id + destination_zone_id + index`, which neither GET nor PUT
  resolves (404 `api.err.FirewallPolicyNotFound`). Of the 88 policies on the
  site at the time, 87 were of that shape and the one real 24-hex id belonged to
  the policy unifig had just created — which carries no `origin_id` at all.

  **So no policy unifig can update carries an `origin_id`, and this ADR's
  sharpest worry is unreachable.** Severing a generated policy's back-reference
  to the network that made it was the loss worth fearing, and there is no request
  unifig can send that would do it.

  What that turned up instead is that unifig cannot update the Controller's own
  policies at all, which is a defect of its own and is **issue #41** rather than
  a consequence of this decision. It is the reason the comment on
  `updateFirewallPolicy` calling a predefined policy "updatable like any other"
  no longer says so.
- `hits` and `last_hit` were the part of that reasoned about rather than
  measured, and the reasoning turned out to be moot rather than wrong: the
  Controller does not take either from the body at all. Sending them a moment
  stale costs nothing, because sending them does nothing. That the counters are
  therefore untouched by any update — rather than merely untouched by this one —
  is inference from the same reading: a field the DTO does not bind on one policy
  is not a field it binds on another.
- **A seventh field was refused, and it was not on anyone's list.** The same
  probe found that an ordinary allow -> block update fails outright, because the
  merge puts the stored `create_allow_respond: true` back beside the new `BLOCK`
  and the Controller refuses that pair (ADR-0022). Every policy the migrated
  router holds carries the flag true, so this was every allow -> block update
  there is. It is the failure mode this ADR named — loud, a 400, the apply
  stopped, nothing written — arriving through a field the issue's table did not
  have, which is the argument for measuring rather than enumerating, made once
  more at unifig's expense. `overwriteManaged` now clears the request on a
  verdict that closes a path, and only clears it: see `clearReturnRuleRequest`.
- The replay stand-in's replace-semantics stops being an assumption. Its comment
  now cites this measurement, which is what ADR-0014's objection to fixtures that
  assert guesses asked for.
- The same shape existed on every v1 Resource unifig updates — a network, a WLAN,
  a port forward all round-tripped through a go-unifi struct, and ADR-0004 said
  the Internal API "stores whatever it is sent rather than merging". Nothing here
  measured those endpoints and this ADR claimed nothing about them. Issue #39
  put the question to a dockerized Controller, and **the answer was the opposite
  one**: a v1 PUT merges, so those updates were dropping nothing and inventing
  eighty-three fields instead. They send only the fields the config states now
  (ADR-0023). Two endpoints, two measurements, two shapes — and the reason this
  ADR filed the question rather than answering it by symmetry.
- What none of this can say is what any of it does on firmware other than
  10.5.67, which is the standing limitation of a single household's router and
  the reason `docs/COMPATIBILITY.md` exists.
