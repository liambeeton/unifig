# Recorded UDR responses

These are the Controller responses the Settings suites replay
(`e2e/replay_test.go`). They exist because the Settings are what the dockerized
Controller cannot stand in for: it has no gateway, so it ships no WAN entries at
all and the suite's usual "seed it and read it back" loop has nothing to seed
onto — and no container can be trusted to say what a Setting looks like on the
firmware an operator actually runs.

One file per endpoint unifig reaches, holding what that endpoint answered —
scrubbed on the way in, and with the fields of each entry sorted (see
"Re-recording" below):

| File                  | Endpoint                                                  |
| --------------------- | --------------------------------------------------------- |
| `sysinfo.json`        | `/proxy/network/api/s/default/stat/sysinfo`                |
| `networkconf.json`    | `/proxy/network/api/s/default/rest/networkconf`            |
| `wlanconf.json`       | `/proxy/network/api/s/default/rest/wlanconf`               |
| `portforward.json`    | `/proxy/network/api/s/default/rest/portforward`            |
| `user.json`           | `/proxy/network/api/s/default/rest/user` (never fetched)   |
| `setting.json`        | `/proxy/network/api/s/default/get/setting`                 |
| `firewallzone.json`   | `/proxy/network/v2/api/site/default/firewall/zone`         |
| `firewallpolicy.json` | `/proxy/network/v2/api/site/default/firewall-policies`     |

`user.json` is the exception to "one file per endpoint unifig reaches": the
stand-in serves it because `export` and `--prune` ask for the client records, but
`make record-udr` never requests it and re-recording leaves it alone. A recording
keeps no client records — a MAC address identifies a piece of hardware for as long
as it exists — and unlike the WLANs and the port forwards, nothing else in the
recorder wants the response either, so it is not fetched rather than fetched and
emptied (ADR-0011). It is committed as an empty collection and stays one.

The last two come from the Controller's **v2** tree and are bare JSON arrays with
no `{"meta": …, "data": …}` envelope around them. That is the Controller's own
shape, and the stand-in reproduces it: a fixture that wrapped a zone list in an
envelope is one unifig's own client cannot read.

`networkconf.json` and `setting.json` are starting states, not scripts: the
replay server holds them in memory and a `PUT` updates its copy the way the
Controller updates its own, which is what lets a test apply a change and then
plan again to prove the apply converged. The writes go to
`rest/networkconf/<id>` for an uplink and `set/setting/doh` for encrypted DNS,
which is the Internal API's own asymmetry rather than a simplification here:
reading one setting means asking for all of them, while writing one names the
key in the path.

## Provenance

**Captured from a physical UniFi Dream Router**, running Network 10.5.67, with
`make record-udr`. The Settings came from a recording on 16 August 2026 and the
firewall from one on **17 August 2026**, taken minutes after the site was
migrated to the zone-based firewall. The uplink was on PPPoE and signed in at
the time, and encrypted DNS was set to a custom resolver, which is what let that
recording answer the two questions
`docs/adr/0008-wan-slots-replay-recorded-responses.md` had left open and the one
in `docs/adr/0012-encrypted-dns-is-a-singleton-setting.md`.

The router has one WAN slot and one custom resolver, so that is what the
recording holds. Nothing in the suite depends on that — see "What the tests need
from a recording" below — and a recording from a router with a second uplink, a
cellular backup or three resolvers drops in the same way.

Earlier versions of these files were recorded from the dockerized Controller and
extended by hand, and this section used to warn you about it. So did a section
about the firewall in particular, which was hand-written for longer than the
rest. Both are kept in the git history rather than here.

### What the firewall recording settled

The firewall was the last part of this recording never to have been near a
router, and replacing it answered three questions that had been guesses — two of
them wrongly, which is the point of having asked.

**Do built-in zones carry `attr_no_delete`? No. No zone has that field at all.**
A zone says it is the Controller's own with `default_zone: true`, alongside a
stable `zone_key` (`internal`, `external`, `gateway`, `vpn`, `hotspot`, `dmz`).
`attr_no_delete` is how a *network* says it — the built-in `Default` network
carries it — and the convention does not carry across. unifig read the network's
marker on a zone, so `--prune` proposed deleting every built-in zone including
the one that stands for the internet; that was issue #23. There is also an
`attr_no_edit`, on `External`, `Gateway` and `Vpn` but not the other three, and
it is not the ownership marker — every zone here is the Controller's own and
only half of them carry it.

**What `attr_no_edit` means is the UI, and nothing at the API.** A membership
PUT to `Vpn`, marked, returned 200, and an independent GET found the new member
really in the zone beside the one no config can name. The Controller reserves
nothing: the marker governs what its own interface offers, not what its API
accepts (ADR-0019, from the write session in #30). This section used to finish
that sentence with "it means editability, not ownership", which reached the
right shelf by dismissing the question rather than asking it — ownership is what
prune needs, editability is what an update needs, and this field turns out to be
neither.

**What the marker did do was correlate with a refusal unifig built itself.**
`go-unifi` models the field as `attr_no_edit,omitempty`, so it goes back out in
the payload exactly when it is true, and the Controller's write DTO answers a
field it has never heard of with `400 JSON parse error: Unrecognized field
"attr_no_edit" ... not marked as ignorable`. `Vpn` and `External` were both
refused where `Dmz` and `Hotspot`, unmarked, took the same request shape — a
correlation with the marker that the marker causes none of. `Gateway`, the third
one carrying it, was never written to, and is expected to refuse for the same
reason rather than known to. A zone's read shape is not
its write shape — `cloud_template`, which all six carry and `go-unifi` does not
model, is refused the same way — and nothing in the type system says so. That is
issue #27: the fix clears the read-only markers before the write, and the
stand-in now answers both measured fields with the 400 a UDR answered, so a
payload the Controller would not parse is no longer one the suite stores and
reads back (ADR-0014).

**Which zones does a UDR ship? Six, not the five that were guessed:** `Internal`,
`External`, `Gateway`, `Vpn`, `Hotspot`, `Dmz`. The guess had no `Dmz` and
spelled `Vpn` as `VPN`. The uplink is in `External` and the LAN in `Internal`, as
expected. Nothing in the suite lists those names any more — the prune test reads
the built-ins out of the recording, because which zones exist is Ubiquiti's to
change.

What the other four hold here is the scrub's answer rather than the router's, and
that is worth knowing before writing a test against it. The router had a LAN in
more zones than this recording has LANs, and a network belongs to exactly one
zone — so `Vpn`, `Hotspot`, `Gateway` and `Dmz` come back empty and only
`Internal` keeps the committed LAN.

Which makes `Default` a poor subject for a test about membership moving, for a
reason that survives the emptying: it is in `Internal`, and `Internal` is the
zone the Controller falls back to for a network nothing else holds (ADR-0020).
A test moving it out of somewhere could not tell "the zone it came from" from
"the zone the Controller sends it to", and would pass on either. Such a test
makes its own network instead — `unzonedNetwork` and `placedNetwork` in
`e2e/firewall_test.go`.

**Does a created policy come back holding what unifig sent?** Unanswered — it
needs a write rather than a read, and these files hold only the Controller's own
policies. Issue #31, answered by the same write probe in #30. It is separated
here from a bigger thing the recording did settle. The Controller ships
**83 predefined policies, one per ordered pair of zones, reusing names across
pairs**: nineteen called "Allow All Traffic", sixteen "Block All Traffic",
twelve "Allow Return Traffic". A policy's identity is therefore its name *and*
its pair of zones, not its name alone — unifig matched on name and refused every
migrated site as ambiguous, which was issue #24. `index` runs from 30000 to
2147483647 on the predefined set, so the old fixture's lone `index: 10000` was a
plausible guess about a value unifig still does not send.

## Re-recording from a real UDR

One command, from the repo root, with `UNIFIG_HOST` and `UNIFIG_API_KEY`
pointing at the router — the same variables unifig itself reads:

```sh
make record-udr
```

It is read-only against the Controller: one GET per file in the table above, and
no other HTTP method anywhere in the program. The worst it can do to a live site
is nothing.

What it does with what comes back:

1. **Answers the questions the ADRs leave open**, if this router can — whether
   `x_wan_password` reads back populated, what `wan_pppoe_username_enabled` /
   `wan_pppoe_password_enabled` hold on a slot that is signed in (ADR-0008),
   whether `sdns_stamp` reads back populated (ADR-0012), and whether any port
   forward is one the Controller owns (ADR-0005). All are read off the responses
   that just arrived. If it prints an answer, put it in the ADR.

   The last of those is the only question asked of a collection this recording
   throws away, so it is answered by counting: how many forwards the router has,
   and how many carry `attr_no_delete` or `predefined`. unifig checks the first
   of those on a forward because the library models the field, not because one
   has been seen carrying it — and a router whose forwards are all deletable is
   the answer that says the exemption never fires. No forward is named, because
   naming one would print what the recording itself refuses to hold.
2. **Scrubs the recording** (`tools/record-udr/scrub.go`, and its tests next to
   it — the code that decides what reaches a public repository is worth
   reviewing on its own).
3. **Stops.** It writes the files, shows you the diff, and asks. Nothing is
   committed by it, ever. Answering no leaves the files there and tells you the
   one command that puts the old recording back.

The raw responses never touch the repository. They go to a temporary directory
outside it and are deleted before the program exits — a raw recording carries
the PPPoE password and the DNS stamps in the clear, which is the whole reason
this is a program rather than a handful of `curl`s and a `jq` filter.

### What the scrub keeps, and what it replaces

Only the **Settings** have to come from your router. The dockerized Controller is
a real Network application and already covers the networks and the WLANs, so
recording yours would prove nothing extra and would publish your subnets, your
VLAN layout and every SSID you broadcast. So the scrub:

- keeps the WAN entries and the Encrypted DNS setting the router sent, including
  every field unifig does not model — those are what the tests exist to protect;
- keeps the firewall zones and policies the Controller marks as **its own**
  (`default_zone` on a zone, `predefined` on a policy), which is exactly what
  unifig's built-in exemption reads and exactly what no container produces. The
  zone marker is not the one a network uses: a network says it with
  `attr_no_delete`, a zone says it with `default_zone` and a stable `zone_key`,
  and reading the network's marker on a zone keeps nothing at all (issue #23). A
  zone or policy you made yourself is dropped rather than scrubbed: it is named
  after your household — the children's tablets, the flat downstairs — and every
  test that wants a custom zone seeds its own;
- points each kept zone's membership at the networks this recording still holds,
  since your LANs are dropped in favour of the committed one. Otherwise the
  zones would come back referring to networks that are not here, and every test
  about what a zone holds would be testing a dangling reference. **One zone gets
  the committed LAN, not each**: a network belongs to exactly one firewall zone
  (ADR-0020), so the first zone your router listed holding a LAN keeps it and the
  rest come back holding none. Folding them independently is what this scrub used
  to do, and it shipped a recording of a site that cannot exist — `Internal`,
  `Vpn` and `Hotspot` all holding the same network. Nothing in the firewall suite
  noticed; what did was `export` writing that membership into a file and
  `validate` refusing it (issue #32);
- takes the LAN from the recording already committed here;
- empties `wlanconf` and `portforward`. Both endpoints still answer, because a
  recording is a statement of what unifig talks to and `export` asks them both —
  but the dockerized Controller holds WLANs and port forwards as well as a router
  does, so a recording has nothing to contribute by keeping yours, and a forward
  is a list of what your house runs, on which machine, reachable from outside;
- keeps nothing else from `get/setting`. That endpoint answers with the whole
  console — mail credentials, RADIUS secrets, remote access, guest portals — and
  unifig reads one key out of it, so the recording holds that one key.

Inside the entries it keeps, it replaces — never removes, because a field that
vanished is a field the tests stopped exercising and nobody would find out:

| What                                                     | Becomes                                          |
| -------------------------------------------------------- | ------------------------------------------------ |
| PPPoE password, WLAN passphrase                           | `recorded-pppoe-password`, …                      |
| Your zone and policy names                                | Not scrubbed — those objects are dropped entirely |
| ISP account name (`wan_username`)                         | `recorded-isp-username`                           |
| DNS stamps (`sdns_stamp`)                                 | `sdns://recorded-dns-stamp-1`, counted            |
| Your name for a resolver (`server_name`)                  | `recorded-dns-server-1`, counted                  |
| Public addresses, gateways, WAN DNS, prefixes             | RFC 5737 / RFC 3849 documentation addresses       |
| The router's MAC                                          | An RFC 7042 documentation MAC                     |
| Console identifiers (`_id`, `site_id`, `sso_app_id`, …)   | Same-shaped placeholders, consistent throughout   |
| Console hostname, display name, timezone                  | `unifi`, `UniFi Console`, `UTC`                   |
| Your name for your connection (`name` on a WAN entry)     | The slot: `WAN`, `WAN2`, `WAN_LTE_FAILOVER`       |

The counted ones are counted so that two resolvers stay two: a fixed placeholder
would record a site with two of them as one written down twice.

An empty field stays empty, a netmask stays a netmask, and `0.0.0.0` still means
"none" — replacing any of those would change what the recording says the router
was configured with.

Last, the scrub goes looking for everything it took out, everywhere in what it
is about to write, and refuses to write anything at all if it finds one. That is
the check that covers the fields it has never heard of. If it refuses, the fix
belongs in `scrub.go`, not in a hand-edited file.

### What only you can check

The diff it stops at is not a formality. The scrub cannot recognise your ISP's
name sitting in prose in a field nobody has seen before, so read the diff for:

- any value that names your ISP, your street, your family or your site;
- any address in it that is actually routed to you;
- anything you would not put on a postcard.

Re-recording an unchanged router produces an unchanged file, so everything in
that diff is a real difference.

## What the tests need from a recording

Only that it holds the site's real Settings. Nothing in either suite names a
slot, a resolver, a WLAN or a secret value that this recording happens to
supply: the tests ask the stand-in which uplinks the recording holds
(`replay.slotNames`, `replay.aSlot`, `replay.absentSlot`), seed the starting
values they need onto a slot every router has (`replay.seedSlot`) or onto the
Encrypted DNS setting every recording has (`replay.seedDoH`), and read the
secrets an export redacted back out of the export's own output rather than
assuming which ones there were. So a re-recording does not have to be arranged
to suit them: run the command, and the suites state the same things about your
router that they state about this one.

What they cannot do without is a `purpose: wan` entry, a `doh` setting and one
LAN: an uplink to test against, a Setting whose shape is the router's own, and
something that is neither to change beside them. The first two come from your
router and the third comes from the committed recording, so the scrub refuses
rather than write a recording missing any of them.

The `doh` setting has to be there, but what is *in* it does not matter: a router
with encrypted DNS switched off records perfectly well, and the DNS tests seed
the state they need onto it. The one thing that trip cannot answer is ADR-0012's
question about whether the stamp reads back, which needs a router with a
resolver actually configured.
