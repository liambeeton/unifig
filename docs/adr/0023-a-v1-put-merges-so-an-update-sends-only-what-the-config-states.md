# A v1 PUT merges, so an update sends only what the config states

A network, a WLAN, a port forward and a DHCP reservation were all updated the
same way: read the live object into a `go-unifi` struct, overwrite the modelled
fields, put the struct back. ADR-0004 said the Internal API "stores whatever it
is sent rather than merging", and #35 measured exactly that on the **v2**
firewall-policy endpoint — where it meant an apply changing a policy's verdict
destroyed the ICMP narrowing an operator had set in the UI, silently (ADR-0021).

Whether the same was true one tree down rested on a sentence nobody had put to a
Controller. ADR-0004's claim is about the create path and reads as an assertion;
its last consequence said so, and filed the question as issue #39. Unlike #35,
#37 and #38 it needed no hardware: networks, WLANs, port forwards and
reservations are exactly the areas the dockerized Controller covers.

## What was measured

Against the dockerized Controller the matrix pins newest —
`linuxserver/unifi-network-application:10.5.67`, a real Network application — on
19 August 2026, as issue #39's probe. Every reading below is the object as the
Controller stores it, read back through its own API rather than through unifig.
Where a finding is dated to another version it is because the whole matrix was
run against — 10.0.162, 10.1.84, 10.4.57 and 10.5.67 — which is how the one rule
that differs between them was found.

**A v1 PUT merges.** A body carrying the object's ID and one field changed that
field and left every other stored field exactly as it was, on all four
collections unifig updates:

| Collection | Body sent | Stored fields before | Changed | Lost |
| --- | --- | --- | --- | --- |
| `rest/networkconf` | `_id`, `vlan` | 19 | `vlan` | none |
| `rest/wlanconf` | `_id`, `x_passphrase` | 32 | `x_passphrase` | none |
| `rest/portforward` | `_id`, `dst_port` | 11 | `dst_port` | none |
| `rest/user` | `_id`, `fixed_ip` | 13 | `fixed_ip` | none |

**A field the body carries wins even at its zero value.** `use_fixedip: false`
with `fixed_ip: ""` set both, on a record holding a live reservation. So a merge
does not cost unifig the ability to clear a field — omitting it is what leaves it
alone, which is the same distinction the config file itself draws (ADR-0004).

**So nothing was being lost, and that is not what was wrong.** The struct round
trip was dropping exactly one field of a network, `external_id`, which `go-unifi`
v2.3.0 does not model — and the merge kept it anyway, because a field that never
reaches the wire is a field the Controller leaves alone. Under the v2 endpoint's
replace that same field would have gone. The two endpoints answer differently,
and neither answer can be read off the other.

**What was wrong is the mirror image, at scale.** Most of the fields `go-unifi`
models carry no `omitempty`, so they go on the wire whether the Controller sent
them or not — and under a merge, a zero in the body is a value stored. An apply
changing one modelled field wrote:

| Kind | Fields the plan printed | Fields written that the Controller had never stored |
| --- | --- | --- |
| Network | 1 | 83 |
| WLAN | 1 | 39 |
| DHCP reservation | 1 | 5 |
| Port forward | 1 | 2 |

They are not nothing. Among a network's eighty-three: `is_nat`,
`internet_access_enabled`, `upnp_lan_enabled`, `network_isolation_enabled`,
`dhcpd_dns_enabled`. A client record gained `network_id`, `fixed_ap_enabled` and
two `virtual_network_override_*` switches; a port forward gained
`src_limiting_enabled`. This is ADR-0021's
`schedule.time_all_day` finding — a Go zero becoming a stored value the operator
never set — three orders of magnitude larger, and it lands on every v1 update
unifig makes. None of it was in the plan, and "a plan that quietly did more than
it printed would not be a plan" is ADR-0004's own sentence.

A Controller-shaped object is what makes the size of it visible. The Controller's
own `Default` network stores **32** fields; the same network after an apply
changing its subnet stored **111**.

**The matrix floor refuses a changed subnet unless the body carries a DHCP
pool.** A `networkconf` body moving `ip_subnet` and naming no
`dhcpd_start`/`dhcpd_stop` is `api.err.Invalid` on **10.0.162**; the same body
with those two keys is taken, and two empty strings satisfy it as well as real
addresses do. **10.1.84, 10.4.57 and 10.5.67 accept it either way.** So this is a
version difference rather than the endpoint's rule, and it was invisible until
now because the struct sent the pair on every write — the first shape of this
change passed the whole suite on 10.5.67 and failed two long-standing network
tests on the floor.

**The Controller reacts to a body as well as storing it.** Writing
`ip_subnet` with its DHCP pool onto that 32-field network took `dhcpdv6_enabled`
off it — with a hand-built body carrying four fields and nothing of unifig's, so
it is the Controller re-deriving its own IPv6 block rather than anything unifig
sent. A merge is therefore not a promise that the stored object is unchanged
apart from the body. What unifig can promise is what it says, and that is the
promise this ADR makes.

## Considered Options

- **Change the comments and leave the code.** Rejected for the reason #35
  rejected it: it documents a defect rather than fixing one, and the defect here
  is on every Resource unifig updates rather than on one.
- **Merge into the object the Controller sent, the way `mergeIntoStoredPolicy`
  does.** The symmetrical answer, and the wrong one. Under a replace that read is
  the only way to keep the operator's fields; under a merge the Controller
  already keeps them, so it buys nothing and costs three things — a second read
  at the moment of writing, a body of a hundred fields where five will do, and
  sending back fields no one has watched these endpoints accept, which is issue
  #37's open worry multiplied by four more endpoints.
- **Send only the fields the config states** — chosen. The body carries the
  Controller's ID and unifig's own values, so what the Controller holds afterwards
  differs from what it held before by exactly what the plan printed. It makes
  ADR-0004's update rule — "reads the live Resource, overwrites only the modelled
  fields and puts the whole object back" — true on the wire, with the Controller
  doing the putting back.

## Consequences

- Five write paths send a `managedUpdate` through `writeManaged` instead of a
  marshalled struct: a network, a WAN slot, a WLAN, a port forward, and a client
  record — the last covering all three of the verbs that touch one, since giving
  an address, moving it and giving it up are one endpoint and differ only in
  which fields the body names.
- **Encrypted DNS is the v1 write not among them, and stays as it is.** It is
  not in the `rest/` tree these four collections share — it is a Setting, written
  through `set/setting/doh` — and a Setting is what the operator's own firmware
  says it is rather than what a container says (ADR-0012), which is why its tests
  replay a recording. Writing the document whole is load-bearing there besides:
  `servers: []` states that a Controller should have no custom resolvers, and
  only a write that replaces the document says so. What that endpoint does with
  a field the body leaves out is a third question, and nothing here answers it.
- Each kind now states its managed fields twice, as the firewall policy already
  did: `overwriteManagedX` for the create, which writes onto unifig's own
  defaults for a new object, and `managedXUpdate` for the update, which is a map.
  They are the same list and a new field unifig comes to own is a change to both.
  The WAN slot has only the second, because nothing creates a WAN slot. Nothing
  checks that the two halves agree, which is a known cost of the split rather
  than an oversight: it is the shape `overwriteManagedPolicy` and
  `storedPolicy.overwriteManaged` already had, and the drift it allows would be
  a field written on create and not on update, silently.
- A network update carries the DHCP pool whenever the subnet it sends differs
  from the one the Controller holds: the rebuilt pool where the change stranded
  the old one, which `relocateDHCP` already worked out and the plan already
  announces, and otherwise the pool the Controller is holding, sent back
  unchanged. An unchanged subnet carries nothing.
- **That leaves two fields unifig writes that nothing asked for**, and they are
  the honest remainder of this change: on a Controller above the floor, a subnet
  change to a network holding no pool stores `dhcpd_start` and `dhcpd_stop` at
  `""`. Sending them is what makes 10.0.162 work, and 10.0 is where the floor is.
  Two is not eighty-three, it is confined to one field's change on one Kind, and
  it is written down here rather than left for the next person to find.
- Clearing a field still works, and that is the reason the body is a map of
  unifig's own rather than a struct: `use_fixedip: false` reaches the Controller,
  where `omitempty` would have elided it. Giving up a reservation is a one-field
  write (ADR-0015).
- `schedule_with_duration` is normalised on the create path only. It was never a
  modelled field — it exists because the Controller refuses `null` — and an
  update that names its fields does not name it.
- **The replay stand-in stops asserting a guess.** Its `update` replaced the
  entry, which was invisible while unifig sent the whole object and wrong the
  moment it stopped. It merges now, citing this measurement, which is what
  ADR-0014 asked of `refusedByZoneWrite` and ADR-0021 did for the v2 side.
- **The WAN slot's evidence is a reading across, and a short one.** No container
  has WAN entries at all (ADR-0008), so no test here can put the question to one.
  But a WAN slot is a `networkconf` entry: the endpoint measured above is the
  endpoint it is written through, and the reading across is between two rows of
  one collection rather than between two endpoints. It is also the update where
  a silent write costs the most, which is why it is not left on the old path
  pending a router.
- ADR-0004's "stores whatever it is sent rather than merging" now carries
  evidence, and the evidence splits it in two. It is **right about create** — a
  network seeded with twelve fields is stored with those twelve and seven of the
  Controller's own, which is why `newNetwork` exists — and **wrong about update**
  on v1. Its consequence about `go-unifi` and `omitempty` is corrected in place.
- What none of this says is what a UDR does. The container is a real Network
  application and these are areas it genuinely covers, which is what made #39
  answerable without hardware — but every measurement in ADR-0019 and ADR-0021
  was taken on a migrated UDR, and a difference between the two would itself be a
  finding. Nobody has looked, and `docs/COMPATIBILITY.md` is where the standing
  limitation lives.
