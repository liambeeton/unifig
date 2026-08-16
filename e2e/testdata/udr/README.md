# Recorded UDR responses

These are the Controller responses the WAN suite replays (`e2e/replay_test.go`).
They exist because a WAN slot is the one thing the dockerized Controller cannot
stand in for: it has no gateway, so it ships no WAN entries at all and the
suite's usual "seed it and read it back" loop has nothing to seed onto.

One file per endpoint unifig reaches, holding what that endpoint answered —
scrubbed on the way in, and with the fields of each entry sorted (see
"Re-recording" below):

| File               | Endpoint                                        |
| ------------------ | ----------------------------------------------- |
| `sysinfo.json`     | `/proxy/network/api/s/default/stat/sysinfo`      |
| `networkconf.json` | `/proxy/network/api/s/default/rest/networkconf`  |
| `wlanconf.json`    | `/proxy/network/api/s/default/rest/wlanconf`     |

`networkconf.json` is the starting state, not a script: the replay server holds
it in memory and a `PUT` updates its copy the way the Controller updates its
own, which is what lets a test apply a change and then plan again to prove the
apply converged.

## Provenance

**Captured from a physical UniFi Dream Router on 16 August 2026**, running
Network 10.5.67, with `make record-udr`. The uplink was on PPPoE and signed in
at the time, which is what let that recording answer the two questions
`docs/adr/0008-wan-slots-replay-recorded-responses.md` had left open.

The router has one WAN slot, so that is what the recording holds. Nothing in
the suite depends on that — see "What the tests need from a recording" below —
and a recording from a router with a second uplink or a cellular backup drops
in the same way.

An earlier version of these files was recorded from the dockerized Controller
and extended by hand, and this section used to warn you about it. It is kept in
the git history rather than here.

## Re-recording from a real UDR

One command, from the repo root, with `UNIFIG_HOST` and `UNIFIG_API_KEY`
pointing at the router — the same variables unifig itself reads:

```sh
make record-udr
```

It is read-only against the Controller: three GETs, and no other HTTP method
anywhere in the program. The worst it can do to a live site is nothing.

What it does with what comes back:

1. **Answers the two questions ADR-0008 leaves open**, if this router can — 
   whether `x_wan_password` reads back populated, and what
   `wan_pppoe_username_enabled` / `wan_pppoe_password_enabled` hold on a slot
   that is signed in. Both are read off the response that just arrived. If it
   prints an answer, put it in the ADR; you are the first person who has had
   one.
2. **Scrubs the recording** (`tools/record-udr/scrub.go`, and its tests next to
   it — the code that decides what reaches a public repository is worth
   reviewing on its own).
3. **Stops.** It writes the three files, shows you the diff, and asks. Nothing
   is committed by it, ever. Answering no leaves the files there and tells you
   the one command that puts the old recording back.

The raw responses never touch the repository. They go to a temporary directory
outside it and are deleted before the program exits — a raw recording carries
the PPPoE password in the clear, which is the whole reason this is a program
rather than three `curl`s and a `jq` filter.

### What the scrub keeps, and what it replaces

Only the **uplinks** have to come from your router. The dockerized Controller is
a real Network application and already covers the networks and the WLANs, so
recording yours would prove nothing extra and would publish your subnets, your
VLAN layout and every SSID you broadcast. So the scrub:

- keeps the WAN entries the router sent, including every field unifig does not
  model — those are what the tests exist to protect;
- takes the LAN from the recording already committed here;
- empties `wlanconf`.

Inside the entries it keeps, it replaces — never removes, because a field that
vanished is a field the tests stopped exercising and nobody would find out:

| What                                                     | Becomes                                          |
| -------------------------------------------------------- | ------------------------------------------------ |
| PPPoE password, WLAN passphrase                           | `recorded-pppoe-password`, …                      |
| ISP account name (`wan_username`)                         | `recorded-isp-username`                           |
| Public addresses, gateways, WAN DNS, prefixes             | RFC 5737 / RFC 3849 documentation addresses       |
| The router's MAC                                          | An RFC 7042 documentation MAC                     |
| Console identifiers (`_id`, `site_id`, `sso_app_id`, …)   | Same-shaped placeholders, consistent throughout   |
| Console hostname, display name, timezone                  | `unifi`, `UniFi Console`, `UTC`                   |
| Your name for your connection (`name` on a WAN entry)     | The slot: `WAN`, `WAN2`, `WAN_LTE_FAILOVER`       |

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

Only that it holds the site's real WAN slots. Nothing in the WAN suite names a
slot, a WLAN or a secret value that this recording happens to supply: the tests
ask the stand-in which uplinks the recording holds (`replay.slotNames`,
`replay.aSlot`, `replay.absentSlot`), seed the starting values they need onto a
slot every router has (`replay.seedSlot`), and read the secrets an export
redacted back out of the export's own output rather than assuming which ones
there were. So a re-recording does not have to be arranged to suit them: run the
command, and the suite states the same things about your router that it states
about this one.

What it cannot do without is a `purpose: wan` entry and one LAN: an uplink to
test against, and something that is not the uplink to change beside it. The
first comes from your router and the second comes from the committed recording,
so the scrub refuses rather than write a recording missing either.
