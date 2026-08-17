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

**The committed fixtures are hand-written, and that is a debt this ADR is booking rather than paying.** ADR-0011 says a recording comes from hardware and a program decides what survives; `make record-udr` now captures both endpoints and scrubs them, so the next run against the maintainer's UDR replaces these files with real ones. Until then the built-in zone set (`External`, `Internal`, `Gateway`, `VPN`, `Hotspot`), the shape of a predefined policy, and above all **whether a built-in zone really carries `attr_no_delete`** are unverified. The last of those is load-bearing: unifig's exemption reads that marker (ADR-0005), and if a real UDR marks built-in zones some other way, prune would propose deleting the zone that stands for the internet. The suite asserts the marker is present before relying on it, so a recording that disagrees fails loudly rather than silently — but a fixture cannot make a router true.

The same gap covers what unifig writes when it *creates* a policy. The Controller rejects one with a null `schedule`, which the exploration established; the rest of `newFirewallPolicy`'s defaults are what the UI is understood to send, and no container refused them because no container would accept any policy at all.

## Considered Options

- **Seed zones and policies into the dockerized Controller's database directly** — rejected on ADR-0008's reasoning, and more clearly here: the Controller refuses to create one through its own API, so seeding means writing to MongoDB behind the application's back. That tests unifig against rows this suite invented, in a Controller that has already said it does not have this feature.
- **Adopt a gateway into the container** — rejected: there is no gateway to adopt. Device adoption is out of scope for this project (issue #1), and a simulated one is not a device that provisions a firewall.
- **Wait for a newer container image** — rejected as a blocker, though it may change the answer later. v10.0.162 is the newest tag published; the maintainer's own router runs 10.5.67. Blocking an area of the v1 catalogue on somebody else's release schedule is the wrong trade when the recording mechanism already exists and was built for exactly this.
- **Ship Zones and Policies untested** — rejected. The area that can lock an operator out of their own network is not the one to leave uncovered, and a recording tests the whole binary end to end even where the fixture's provenance is weaker than it should be.
- **Test at a code seam instead, with a fake client** — rejected: the spec ruled out code-level mocking once and for all, and it would stop exercising the v2 URL layout and the bare-array decoding, which is most of what is new here.

## Consequences

- The compatibility matrix (issue #11) cannot say anything about Zones or Policies from CI alone, and should not pretend to. Firewall coverage is a claim about a recording until a container ships the feature.
- `make record-udr` reads six endpoints rather than four, and its scrub keeps only the zones and policies the Controller marks as its own — `attr_no_delete` and `predefined`. A zone an operator made is named after their household, and the tests that want a custom zone seed their own. Not recording something remains the only rule that cannot be got wrong (ADR-0011).
- A zone's membership is rewritten during the scrub rather than merely substituted: the router's own LANs are dropped in favour of the committed one, so a member naming a dropped LAN is pointed at the LAN the recording keeps. Without that, every zone would come back holding ids that resolve to nothing.
- **Re-recording from the UDR is the acceptance test this issue could not run.** It should be done before v1 (issue #13), and the questions to answer while a real router is on the other end are in `e2e/testdata/udr/README.md`: whether built-in zones carry `attr_no_delete`, which zones a UDR actually ships, and whether a created policy comes back holding the fields `newFirewallPolicy` sent.
