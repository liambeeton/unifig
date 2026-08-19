# A v2 zone PUT merges, and its write DTO takes three fields

ADR-0021 measured the v2 firewall-policy endpoint **replacing**, and fixed a
live-path defect by having an update carry back every field the Controller sent.
It deliberately claimed nothing about the zone endpoint, and said why: the only
reading that existed pointed the other way, and inferring one v2 collection's
behaviour from another's would be inferring across two DTOs already known to
differ (ADR-0019, issue #27).

The reading it declined to trust was step 0 of #35's probe, which read all six
built-in zones still carrying every field unifig's payload could not have
contained. That looks like a merge and is not evidence of one: every survivor is
a field the Controller could regenerate for a **built-in** zone — `zone_key` is
the zone's own identity, `default_zone` and `attr_no_edit` follow from being
built-in, and `cloud_template` is `null` everywhere. The one value nothing could
reconstruct is `external_id`, and the recording's are scrubbed to placeholders,
so there was no pre-write value to compare against.

Settling it wanted a **custom** zone, whose `external_id` the Controller cannot
regenerate. That is issue #38, and this is what it measured.

## What was measured

Against the live migrated UDR on Network 10.5.67 (`UDR.mt7622.v5.1.19`), 19
August 2026. A throwaway custom zone `unifig-probe-38` was created holding no
networks — so nothing rode it in either direction, and no network was moved at
any point — and deleted at the end. The site went 6 zones and 86 policies to 7
and 103 and back to 6 and 86, byte-identical to baseline, with every policy id
the same: the Controller reclaimed the 17 policies it generated for the new
zone's pairs, as ADR-0019 measured it doing for #30's.

**A v2 zone PUT merges.** The zone was created carrying
`external_id: 28a563df-4bdc-4f9f-b795-76b4ea54dbf1`, `zone_key: null`,
`default_zone: false` and `cloud_template: null`. A PUT carrying exactly the
three fields unifig sends —

```json
{"_id": "…", "name": "unifig-probe-38-renamed", "network_ids": []}
```

— returned 200, and an independent GET afterwards showed the new name beside the
**same `external_id`**. The rename is what makes it a measurement rather than a
coincidence: it proves the write executed rather than being short-circuited as a
no-op, which a membership PUT of the same value could not have. A UUID on a
custom zone is the one field in a zone's read shape the Controller has no way to
regenerate, and it was still there after a body that did not carry it.

**The write DTO takes `_id`, `name` and `network_ids`, and nothing else.** Each
remaining field of the read shape was then PUT on its own, on top of that same
three-field body, and every one was refused:

| field sent | answer |
| --- | --- |
| `external_id` | 400 `Unrecognized field "external_id"` |
| `zone_key` (as `null`, and as a string) | 400 `Unrecognized field "zone_key"` |
| `default_zone` | 400 `Unrecognized field "default_zone"` |
| `cloud_template` | 400 `Unrecognized field "cloud_template"` |
| `attr_no_edit` (as `false`) | 400 `Unrecognized field "attr_no_edit"` |
| `site_id` | 400 `Unrecognized field "site_id"` |

all naming the same class, `com.ubnt.g.c.t.AWSXjrFfvsFZsv`, as #30's refusal did.
Two of these sharpen what ADR-0019 could say. `attr_no_edit` was refused sent as
`false` — the value `omitempty` hides — so the DTO refuses the **field** rather
than the value, and the correlation with marked zones was never anything but
`omitempty`'s doing. And `site_id` is not in a zone's read shape at all.

## The two findings are one mechanism

Merging into the stored object, which is what saved the policies, is not a
request a zone can make: every field such a body would carry back is a field
this endpoint answers 400 to. Had the zone endpoint replaced, unifig would have
had no way to stop it — `external_id` would be lost on every membership change
and the only honest fix would have been the comment.

It does not replace, and the same minimality is why. This DTO takes the three
fields that are the operator's to set and refuses everything the Controller owns,
so there is no body that could delete one; a field left out is left alone
because leaving it out is the only thing a client may do with it. The policy
endpoint accepts the Controller's own fields back (issue #37) and replaces, and
those two facts fit together the same way from the other side.

## Consequences

- **`updateZone` was already correct, and is correct necessarily rather than by
  luck.** The comment promising that what a zone carries beside unifig's fields
  survives an apply is true. It hedged that promise with the fields the body
  could not carry; the hedge comes off, because those are exactly the fields the
  Controller keeps. #38's acceptance offered "fix it or stop promising it", and
  the answer is neither.
- **One field was on the wire that should not have been.** `writableZone` now
  clears `SiteID` alongside the four markers. go-unifi models it as
  `json:"site_id,omitempty"` and no GET this repository has seen returns it, so
  it has been eliding by luck — the same defect as `attr_no_edit` with the sign
  flipped, where the field went out exactly when it was true and this one stays
  home exactly while the Controller stays silent. A firmware that answered with
  a `site_id` would have 400'd every zone update, found by whoever was holding
  it. This is the only code change #38 produced and it came from the refusal
  table rather than from the question the issue asked.
- **The probe sent a hand-built body, not an apply, and the gap that leaves is
  closed by a test rather than left open.** #38's wording asked for a network to
  be added through `unifig apply`; this ran a zone holding no networks instead,
  so nothing rode it and nothing was moved off a live LAN to find out. What the
  hand-built PUT then proves is a fact about the *endpoint*, and it only reaches
  unifig if unifig's own body is the same three fields. That is now asserted
  directly — `TestTheZoneUnifigWritesBackCarriesOnlyTheThreeFieldsItsDTOTakes`
  pins the update's body as an exact set — so the chain is: the test says what
  unifig sends, the router says what happens to it. Neither half is an
  assumption, and the exact set is what will catch the next field go-unifi
  models without `omitempty`, which is the failure this ADR and ADR-0019 have
  now both been written about.
- **`refusedByZoneWrite` grows from two fields to six**, and the stand-in now
  refuses what hardware refuses rather than a measured subset of it. That is what
  caught the `site_id` defect: the existing zone tests seed a zone through
  `seedZone`, which stamps one, and they had been passing while unifig sent it.
- **The replay stand-in merges on zones and replaces on policies**, per endpoint,
  each on its own measurement. Its comment named this the one place it was
  deliberately stricter than the Controller might turn out to be, and the
  argument for that — a merging stand-in would store what unifig failed to send
  and read it back as a success — does not survive: `refusedByZoneWrite` now
  answers 400 to every field this DTO does not take, so there is no payload a
  merge could launder. ADR-0014's objection to a fixture that asserts a guess is
  met on both collections now.
- **"v2 replaces" was never the rule, and this is the second time this
  repository has learned the same thing.** The axis is the endpoint, not the API
  version and not the verb: two v2 collections on one Controller, measured a day
  apart, answer oppositely. ADR-0021 filed this question rather than reasoning it
  out by symmetry, and had it reasoned it out it would have been wrong.
  CONTEXT.md's Internal API entry says which endpoint each measurement covers.
- What none of this can say is what any of it does on firmware other than
  10.5.67, which is the standing limitation of a single household's router and
  the reason `docs/COMPATIBILITY.md` exists.
