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
| `setting.json`        | `/proxy/network/api/s/default/get/setting`                 |
| `firewallzone.json`   | `/proxy/network/v2/api/site/default/firewall/zone`         |
| `firewallpolicy.json` | `/proxy/network/v2/api/site/default/firewall-policies`     |

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

**Captured from a physical UniFi Dream Router on 16 August 2026**, running
Network 10.5.67, with `make record-udr`. The uplink was on PPPoE and signed in
at the time, and encrypted DNS was set to a custom resolver, which is what let
that recording answer the two questions
`docs/adr/0008-wan-slots-replay-recorded-responses.md` had left open and the one
in `docs/adr/0012-encrypted-dns-is-a-singleton-setting.md`.

The router has one WAN slot and one custom resolver, so that is what the
recording holds. Nothing in the suite depends on that — see "What the tests need
from a recording" below — and a recording from a router with a second uplink, a
cellular backup or three resolvers drops in the same way.

An earlier version of these files was recorded from the dockerized Controller
and extended by hand, and this section used to warn you about it. It is kept in
the git history rather than here.

### Except the firewall, which has not been recorded yet

`firewallzone.json` and `firewallpolicy.json` are **hand-written**, and are the
one part of this recording that has never been near a router. They were added
with the zone-based firewall (issue #8) because the dockerized Controller has no
such firewall at all — see
`docs/adr/0013-the-firewall-cannot-be-tested-against-a-container.md` — so there
was no other way to test the area, and no hardware to hand at the time.

What they assume, and what a real recording would settle:

- **that a built-in zone carries `attr_no_delete`.** This is the load-bearing
  one. unifig's exemption reads that marker (ADR-0005), so if a UDR marks its
  built-in zones some other way, `--prune` would propose deleting the zone that
  stands for the internet. The suite asserts the marker is present before
  relying on it, so a recording that disagrees fails loudly — but a fixture
  cannot make a router true.
- **which zones a UDR actually ships**, and what each holds. The fixture has
  `External`, `Internal`, `Gateway`, `VPN` and `Hotspot`, with the uplink in
  `External` and the LAN in `Internal`.
- **what a predefined policy looks like**, and whether a policy unifig creates
  comes back holding the fields it sent (`newFirewallPolicy` in
  `internal/reconcile/policy.go`). The Controller is known to reject a policy
  with a null `schedule`; the rest of those defaults are unverified. `index` is
  the one to look at first: the hand-written fixture carries `index: 10000` and
  unifig sends none, on the assumption the Controller assigns it.

If you are the one running `make record-udr` against a real UDR, these are the
questions to answer while you have one. Replacing these two files is the
acceptance test issue #8 could not run.

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
   `wan_pppoe_password_enabled` hold on a slot that is signed in (ADR-0008), and
   whether `sdns_stamp` reads back populated (ADR-0012). All are read off the
   responses that just arrived. If it prints an answer, put it in the ADR.
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
  (`attr_no_delete` on a zone, `predefined` on a policy), which is exactly what
  unifig's built-in exemption reads and exactly what no container produces. A
  zone or policy you made yourself is dropped rather than scrubbed: it is named
  after your household — the children's tablets, the flat downstairs — and every
  test that wants a custom zone seeds its own;
- points each kept zone's membership at the networks this recording still holds,
  since your LANs are dropped in favour of the committed one. Otherwise the
  zones would come back referring to networks that are not here, and every test
  about what a zone holds would be testing a dangling reference;
- takes the LAN from the recording already committed here;
- empties `wlanconf`;
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
