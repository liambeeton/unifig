# WAN slots are tested against recorded Controller responses

The spec (issue #1) chose the substitution level once and for all: the Controller is replaced at the network level, behind the same base URL, and never at a code seam. For the six areas a container can exercise that means the dockerized Controller. WAN is the exception it named in advance, and the reason turns out to be stronger than "a container has no gateway": a demo-mode Network 10.0.162 site's `networkconf` collection contains exactly one entry, the built-in `Default` LAN. There are no WAN slots to match, none to update, and nothing a seeded LAN could honestly stand in for — a `purpose: wan` entry created by hand is a row this suite invented, not an uplink the router has.

So the WAN tests run against a recorded Controller: the responses to the three endpoints unifig reaches, served over HTTPS behind the same base URL, through the same API-key header, to the same real binary (`e2e/replay_test.go`, `e2e/testdata/udr/`). Nothing in unifig knows the difference, which is what makes the substitution honest — the seam is still the process boundary, and what changed is which Controller answers.

The recording is **stateful for writes**: a `PUT` replaces the stand-in's copy of an entry the way the Controller replaces its own. A pure replay could not state this suite's central sentence — apply converged, and the next plan is empty — because the second plan would read back the first plan's world. Storing what unifig sent is also what lets a test prove unifig writes the whole entry back rather than a struct of its own.

**The recordings are not yet from the physical UDR**, and that is the gap this leaves open. They were captured from the same dockerized Controller the rest of the suite runs against and extended by hand with WAN slots, using the field names and values that Controller returns for a seeded `purpose: wan` entry. So they prove unifig handles the shape — matching a slot, diffing its fields, preserving what it does not model — and they do not yet prove a real UDR's factory slots look like this.

This is the third assumption deferred off the real hardware (ADR-0003 for auth, ADR-0007 for secret readback), and it is recorded rather than closed. `e2e/testdata/udr/README.md` carries the one command that closes it, secrets scrubbed on the way through. Two specific things it would confirm:

- **PPPoE credentials read back in the clear.** `x_wan_password` comes back byte-identical on a plain `GET /rest/networkconf` from the dockerized Controller, so ADR-0007's finding holds for the second secret unifig models and no write-only fallback is built. A UDR that withheld it would make every WAN plan non-empty, which the idempotence tests catch immediately rather than subtly.
- **`wan_pppoe_username_enabled` / `wan_pppoe_password_enabled`.** unifig switches these on when it writes the credential beside them, on the same reasoning as a WLAN passphrase implying WPA-PSK: a credential stored next to a flag saying it is unused is an uplink that quietly does not sign in. The Controller stores the two independently, so which combination the UDR actually acts on is a fact only the UDR has.

Which field identifies a slot is a separate decision, taken against the same recordings and recorded in ADR-0010. How much of a recording has to come from the router at all — and what is left of an uplink once the household is taken out of it — is ADR-0011, which is also where the command named above ended up living.

## Considered Options

- **Seed a `purpose: wan` entry into the dockerized Controller and treat it as a WAN slot** — rejected. It tests unifig against a row this suite made up, so every field the real router populates and unifig must preserve is a field the test never sees. Worse, it would pass whatever the UDR does.
- **Record and replay at the SDK's client interface** — rejected: it is the code-level mock the spec ruled out, and it would stop exercising the URL layout, the API-key gate and the SDK's own API-style probe, which is most of what a UDR's own layer does differently.
- **Skip WAN in CI and test it by hand against the router** — rejected: the one area that can take a household off the internet is the last one to leave untested, and a manual check does not run on every commit.
- **Wait for a real recording before building the stand-in** — rejected: the mechanism is the part that takes work, the recording is one command, and blocking six other areas of test infrastructure on hardware access would be the wrong order.

## Consequences

- The WAN tests need no Controller container of their own — they use the suite's rig only for the binary it builds, and the container the package's `TestMain` boots is there for their neighbours. A recording is a file, so a second Controller version's WAN behaviour is a second file rather than a second container.
- Anything unifig starts asking the Controller for shows up as an unrecorded endpoint and fails loudly, naming the request. That is a feature: the recording is a statement of what unifig talks to, and it should not change quietly.
- Encrypted DNS (issue #7) is the second Setting and inherits all of this — the same stand-in, the same file layout, the same provenance gap to close in the same command.
- Static addressing, DS-Lite and MAP-E are not modelled, because each needs fields the config has no way to state. A slot configured that way exports as its slot alone and unifig manages nothing about it, which is a smaller promise than pretending to and a truthful one.
