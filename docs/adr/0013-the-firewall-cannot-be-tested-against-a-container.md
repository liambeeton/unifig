# The zone-based firewall is tested against a recording, because no container has one

Issue #8 asked for Zones and Firewall Policies "covered by dockerized-Controller tests at the process-level seam". They are not, and cannot be. The dockerized Controller has no zone-based firewall at all.

What was actually found on `jacobalberty/unifi:v10.0.162`, the newest tag that image publishes and the one CI pins:

- `GET /v2/api/site/<site>/firewall/zone` answers `200 []`. So does `firewall-policies`, and so does `zone-matrix`. Not an error — an empty collection.
- A **freshly created site** answers the same way, so this is not leftover state on the demo site. There is no point at which a Controller of this version produces a built-in zone.
- `POST /v2/api/site/<site>/firewall/zone` is refused with `404 api.err.CouldNotFindHotspotFirewallZone`. The Controller will not create a zone until the built-in set exists, and nothing in the container ever creates it.
- `described-features` never lists `ZONE_BASED_FIREWALL`, and `stat/device` is empty: demo mode simulates a gateway in the site's dashboard, but no gateway is ever adopted.

The zone-based firewall is a property of the gateway, and it is provisioned when a real one is adopted. A container without one does not have a smaller firewall; it has none.

**This contradicts the parent spec, not just issue #8's checklist**, and it is worth saying plainly rather than leaving to be discovered. Issue #1's Testing Decisions put Zones and Policies on the container side in as many words: "a real dockerized UniFi Network application for everything a container can exercise (networks, WLANs, Zones, Policies, port forwards, DHCP Reservations)". That sentence was written before anyone had asked a container for a zone. It is wrong about two of the six, and the same paragraph's other half — "recorded real-UDR HTTP responses replayed by a local test server for what a gateway-less container cannot" — is the rule that actually applies to them. Issue #1 should be amended to move Zones and Policies across; this ADR is not a licence to leave the spec saying something untrue.

That makes this ADR-0008 arriving at a second area for the same underlying reason, and the same answer follows: the Controller is still substituted at the network level, behind the same base URL, through the same API-key header, to the same real binary — what changes is which Controller answers. The firewall suite runs against `e2e/replay_test.go` with `firewallzone.json` and `firewallpolicy.json` alongside the WAN and Encrypted DNS fixtures.

Two things about that stand-in are new rather than inherited. It serves the Controller's **v2 tree**, whose responses are bare JSON arrays with no `{meta, data}` envelope — the first shape difference the recording has had to carry. And it is the first stand-in here to **create and delete** rather than only update, because Zones and Policies are the first Resources tested this way rather than Settings; it hands out an ID on create, which is exactly what a policy planned onto a new zone has to be able to read.

**The committed fixtures were hand-written, and this ADR booked that as a debt rather than paying it.** ADR-0011 says a recording comes from hardware and a program decides what survives; `make record-udr` captures both endpoints and scrubs them. The debt was paid on 17 August 2026 and the section below records what it bought — read it before the paragraph that follows, which is kept as written because being wrong in public is the point of it.

What this ADR said while the fixtures were hand-written: the built-in zone set (`External`, `Internal`, `Gateway`, `VPN`, `Hotspot`), the shape of a predefined policy, and above all **whether a built-in zone really carries `attr_no_delete`** are unverified. The last of those is load-bearing: unifig's exemption reads that marker (ADR-0005), and if a real UDR marks built-in zones some other way, prune would propose deleting the zone that stands for the internet. The suite asserts the marker is present before relying on it, so a recording that disagrees fails loudly rather than silently — but a fixture cannot make a router true.

That paragraph was right about the risk, wrong about the zone set, and wrong about the safeguard. The suite did assert the marker before relying on it — against a fixture written in the same change to carry it, so the assertion could not fail. A test that states an assumption is only worth what its fixture's provenance is worth.

The same gap covers what unifig writes when it *creates* a policy. The Controller rejects one with a null `schedule`, which the exploration established; the rest of `newFirewallPolicy`'s defaults are what the UI is understood to send, and no container refused them because no container would accept any policy at all.

## What the maintainer's UDR answered

**Answered on 17 August 2026, and the debt above is paid.** The site was migrated
to the zone-based firewall and `make record-udr` was run against it, on the same
UDR running Network 10.5.67 that ADR-0007, ADR-0008 and ADR-0012 were closed
against. `e2e/testdata/udr/firewallzone.json` and `firewallpolicy.json` now hold
six zones and eighty-three predefined policies that came from hardware.

It took two runs. The first, before the migration, found the router still on the
**legacy firewall**: `firewall/zone`, `firewall-policies` and `zone-matrix` all
answered `200 []`, `rest/firewallrule` held four rules, and `described-features`
listed `ZONE_BASED_FIREWALL_MIGRATION` rather than `ZONE_BASED_FIREWALL`. The
zone-based firewall arrives with an adopted gateway, but a site carried forward
from before it existed stays on the legacy rules until somebody migrates it. So
a UDR is necessary and not sufficient; a **migrated** UDR is the requirement, and
that is worth knowing before arranging access to hardware.

Two of the three deferred questions turned out to be answered wrongly, which is
the whole return on the exercise:

- **A built-in zone does not carry `attr_no_delete`.** No zone has the field. The
  marker is `default_zone`, with a stable `zone_key` beside it. The load-bearing
  fear in this ADR was correct: unifig read the network's marker on a zone, so
  `--prune` proposed deleting every built-in including `External`. Filed and
  fixed as issue #23; ADR-0005 now records that the marker is per Resource.
- **A policy's identity is its name and its pair of zones, not its name.** The
  Controller ships its predefined policies one per ordered zone pair and reuses
  names across them — nineteen "Allow All Traffic", sixteen "Block All Traffic".
  unifig matched on name and refused every migrated site as ambiguous, telling
  the operator to rename policies that were not theirs to rename. Filed and fixed
  as issue #24, on ADR-0001's per-type natural keys.
- **The built-in set is six, not five**: `Internal`, `External`, `Gateway`,
  `Vpn`, `Hotspot`, `Dmz`. The guess had no `Dmz` and spelled `Vpn` differently.
  The prune tests no longer name any of them — they read the built-ins out of
  the recording, so a firmware that adds a seventh does not make them wrong.
  Other tests still name `Internal` and `External` deliberately: a test about
  what a zone holds needs a zone it can point at, and those two are the ones the
  recording is known to hold. What matters is that nothing *asserts the set*.

Both defects had the same shape, and it is the shape this ADR predicted. Each was
a guess about someone else's product, written into a fixture in the same change
as the code that read it, so the test asserted the guess against itself and
passed. Neither could fail until the fixture came from hardware. **A hand-written
fixture cannot fail a test about the thing it was written to describe** — which
is the argument for ADR-0011 stated as a result rather than as a principle.

What is still unanswered is the narrowest of the three: whether a policy unifig
*creates* comes back holding the fields `newFirewallPolicy` sent. The recording
holds only the Controller's own policies, and answering it needs a write to a
real site rather than a read.

## Considered Options

- **Seed zones and policies into the dockerized Controller's database directly** — rejected on ADR-0008's reasoning, and more clearly here: the Controller refuses to create one through its own API, so seeding means writing to MongoDB behind the application's back. That tests unifig against rows this suite invented, in a Controller that has already said it does not have this feature.
- **Adopt a gateway into the container** — rejected: there is no gateway to adopt. Device adoption is out of scope for this project (issue #1), and a simulated one is not a device that provisions a firewall.
- **Wait for a newer container image** — rejected as a blocker, though it may change the answer later. v10.0.162 is the newest tag published; the maintainer's own router runs 10.5.67. Blocking an area of the v1 catalogue on somebody else's release schedule is the wrong trade when the recording mechanism already exists and was built for exactly this.
- **Ship Zones and Policies untested** — rejected. The area that can lock an operator out of their own network is not the one to leave uncovered, and a recording tests the whole binary end to end even where the fixture's provenance is weaker than it should be.
- **Test at a code seam instead, with a fake client** — rejected: the spec ruled out code-level mocking once and for all, and it would stop exercising the v2 URL layout and the bare-array decoding, which is most of what is new here.

## Consequences

- The compatibility matrix (issue #11) cannot say anything about Zones or Policies from CI alone, and should not pretend to. Firewall coverage is a claim about a recording until a container ships the feature.
- `make record-udr` reads six endpoints rather than four, and its scrub keeps only the zones and policies the Controller marks as its own — `default_zone` and `predefined`. It read `attr_no_delete` on a zone until migrated hardware showed no zone carries it (issue #23). A zone an operator made is named after their household, and the tests that want a custom zone seed their own. Not recording something remains the only rule that cannot be got wrong (ADR-0011).
- A zone's membership is rewritten during the scrub rather than merely substituted: the router's own LANs are dropped in favour of the committed one, so a member naming a dropped LAN is pointed at the LAN the recording keeps. Without that, every zone would come back holding ids that resolve to nothing.
- **Re-recording from a migrated UDR was the acceptance test issue #8 could not run, and it has now been run.** This bullet has said three things in two days: that it should happen before v1, then that v1 would carry the debt rather than block on a change to the maintainer's home network, and now that the recording exists. The middle version was written while the router was on the legacy firewall and the migration looked like it would not happen; it is worth remembering that the argument for shipping the debt was reasonable and would have shipped two defects that only hardware could find.

  **What it cost to be wrong here was two bugs, both in the direction of destroying an operator's configuration.** `--prune` proposing the deletion of every built-in zone (#23), and every migrated site refused as ambiguous (#24). Neither was reachable by any test this repository could run, because the fixtures asserting otherwise were written by the same hand as the code. Firewall coverage is now a claim about hardware, and the compatibility matrix (issue #11) can say so for the recording's version — 10.5.67 — while still saying nothing from CI alone, since the container has no firewall to test.
