# A recording carries only the uplinks, and a program decides what is left of them

ADR-0008 put the WAN tests on recorded Controller responses and left the recording itself undescribed: three curl commands, a `jq` filter replacing two secrets, and a sentence in a README asking the operator to read the diff. Two things were wrong with that, and they are the same thing said twice — nothing had decided **how much of a recording has to be real**.

**Only the uplinks do.** The dockerized Controller is a real Network application, and it already covers the networks and the WLANs the rest of the suite exercises. The one thing a gateway-less container cannot produce is a `purpose: wan` entry. So a recording contributes its WAN entries and nothing else: the LAN comes from the recording already committed, and `wlanconf` is emptied. Recording the operator's networks and SSIDs would prove nothing the container does not already prove, and would cost their subnets, their VLAN layout, every SSID they broadcast and the name of the house.

**What is left of an uplink is decided by a program, not by prose.** `tools/record-udr/scrub.go` is the whole of it, with its tests beside it, because the code that decides what reaches a public repository is exactly the code that should be reviewable on its own and provable by a test rather than by whoever last read the README carefully. Its rules: credentials, ISP addressing, console identifiers and the operator's own name for their connection are **replaced** by placeholders of the same type — an address by a documentation address, a UUID by a UUID, an empty field by an empty field. Never removed. A field that vanished is a field the tests stopped exercising, and nobody would find out until a real UDR behaved differently from the recording and the suite said nothing.

The rules are two tables and a shape check, so they cover the fields somebody thought of. What covers the rest is the last step: everything the scrub took out, it looks for again in what it is about to write, everywhere, and it writes nothing at all if it finds one. An ISP's name in a field this project has never seen stops the re-recording rather than reaching a diff nobody reads closely enough. That failure is a bug report against the scrub, and it is one this repository can fix once for everybody.

Around the scrub, the command (`make record-udr`) is read-only against the router — three GETs, no other method in the program — keeps the raw responses outside the repository and deletes them before exiting, and stops at the diff. Nothing in it commits. While it is there and a real router is on the other end, it also asks ADR-0008's two open questions of the response that just arrived, because that is the cheapest moment anyone will ever have to answer them.

## Considered Options

- **A hardened `jq` filter in the README** — rejected, and it is what this replaces. A filter in prose is not reviewed, not tested, and not run the same way twice; the one that shipped scrubbed two fields and left the ISP account name, the public address and every SSID in place.
- **Record everything the router returns and scrub harder** — rejected: it makes the privacy of a public repository depend on a denylist being complete, when the container can already stand in for everything except the uplinks. Not recording something is the only rule that cannot be got wrong.
- **Redact by dropping the field** — rejected: it silently narrows what the suite tests, and the narrowing is invisible in a diff full of removals that all look deliberate.
- **Anonymise with random values per run** — rejected: a re-recording of an unchanged router would produce a diff full of noise, and the diff is the one place a human is asked to look.
- **Generate the recording from the go-unifi field spec instead of from a router** — rejected: that is the hand-extended recording ADR-0008 already has, and the gap it leaves open is exactly that nobody has seen a real UDR's factory slots.

## Consequences

- Re-recording an unchanged router is an empty diff: placeholders are handed out in a fixed order, and the files are written with sorted fields. Everything in that diff is a real difference.
- The recording's field order stops matching the Controller's. Nothing reads it — the replay stand-in and unifig both parse JSON objects — and a stable order is what makes the diff readable.
- The suite is now provably indifferent to which router the recording came from: it was run against a scrubbed recording of a router with PPPoE on the primary slot, no `WAN2`, an LTE failover slot, a renamed connection and a WAN VLAN, which is the acceptance criterion issue #16 left behind.
- Encrypted DNS (issue #7) inherits this the way it inherits the stand-in: a fourth endpoint is one more entry in `endpoints`, and its secret is one more line in the scrub's table.
- A recording is now cheap enough to make that a second Controller version's WAN behaviour is one command rather than a project.
