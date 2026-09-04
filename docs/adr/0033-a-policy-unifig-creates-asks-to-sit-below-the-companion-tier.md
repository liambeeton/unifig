# A policy unifig creates asks to sit below the companion tier

unifig was creating firewall policies that outranked the companion return rules
of its own allows, and there was no config an operator could write to avoid it.
A policy that blocks now names the index it wants. Whether the Controller honours
that is unmeasured, so unifig reads the answer back and says so where the answer
is no.

## What was measured

Live UDR running UniFi Network 10.6.101, 137 policies, 3 September 2026 (issue
#54). The config stated the ordinary pair of intentions — trusted reaches IoT,
IoT does not reach trusted:

```yaml
- name: Gibson to Ellingson      - name: Ellingson off the Gibson
  action: allow                    action: block
  source: Gibson                   source: Ellingson
  destination: Ellingson           destination: Gibson
```

`unifig plan` returned exit 0, no changes. Both policies were live and exactly as
written. What the Controller held on that pair, in evaluation order:

```
10000        Ellingson -> Gibson     'Ellingson off the Gibson'      BLOCK  ALL
10000        Gibson    -> Ellingson  'Gibson to Ellingson'           ALLOW  ALL
30000        Ellingson -> Gibson     'Gibson to Ellingson (Return)'  ALLOW  RESPOND_ONLY
2147483647   Ellingson -> Gibson     'Block All Traffic'             BLOCK  ALL
```

The companion was created correctly — `create_allow_respond: true`,
`predefined: true`, index 30000, exactly as ADR-0022 and ADR-0026 measured. It is
never reached: the operator's own block sits 20000 above it. A second instance on
the same site, unnoticed until it was looked for: `Management to Ellingson
(Return)` at 30000 under `Ellingson off management` at 10000.

**Confirmed at packet level in the same session**, one variable moved — `enabled`
on the reverse block, written with a v2 PUT carrying the whole stored object back
and restored afterwards:

```
block ENABLED   (baseline)   ping 10.15.7.202 …  100% loss
                             18 TCP connects across 7 hosts … all TIMEOUT, zero RST
block DISABLED  t=10s        REPLY  (sweep of five hosts, all REPLY)
block RE-ENABLED             no reply
```

Two details from that probe are worth carrying: the dataplane takes about ten
seconds to reload, so a three-second settle reads as a clean refutation of the
whole hypothesis; and the same site holds a natural control in `Gibson to
management`, an allow with a companion and **no reverse block**, which answered
throughout.

## Why it is unifig's problem

Every stored policy unifig created landed at `index: 10000` — the value the
Controller assigns unasked, recorded as `storedPolicyIndex` and confirmed on all
nine stored policies on that site. Every companion landed at 30000. So unifig
created blocks that structurally outranked the companions of its own allows, and
`index` was not among the five fields `overwriteManaged` owns, so there was
nothing an operator could write.

The Controller's own rule set is laid out on the opposite principle. In the
86-policy 10.5.67 recording, nine of the twelve `Allow Return Traffic` policies
sit at 30000 with every generic block on the same pair *below* them — `Block
Invalid Traffic` at 30001, `Block All Traffic` at 2147483647. Return traffic is
admitted before blocks are applied.

The three exceptions prove it is a convention rather than an accident: on
`Hotspot -> Internal` and `Hotspot -> Vpn`, `Post-Authorization Restrictions`
(30001) deliberately sits *above* `Allow Return Traffic` (30002), because a
captive portal has to drop replies until a client authenticates. That is what an
intentional override of the convention looks like. unifig's was unintentional.

## The value, and which policies ask for it

`unifigPolicyIndex` is **40000**, asked for on a create whose verdict closes a
path — `block` or `reject` — and on no other.

The band the Controller generates into is **per pair rather than per site** —
every pair in the recording starts again at 30000 — and it runs 30000 to 30008
before jumping to the catch-all at 2147483647. Three constraints pick the value
out of that:

- **Above the companion tier at 30000**, which is the whole point: the return
  rule has to be reached before a policy unifig created is.
- **Above the rest of the generated band**, so the value ties with no index
  anyone has read off a Controller. A tie is an ordering nobody has measured, and
  this project does not ship one of those.
- **Below the catch-all at 2147483647**, so ADR-0018 and ADR-0029 keep the
  arguments they rest on: a policy the operator creates over the Controller's own
  still takes effect, and a stored policy still outranks the generated one sharing
  its key.

### Only a blocking verdict asks, and the recording is why

The first draft of this moved **every** policy unifig creates to 40000, on the
argument that placing an `allow` differently from a `block` would be unifig
sorting the operator's own policies among themselves. The recording refutes it.

The generated tier is not all return rules. `Isolated Networks` is an enabled
`BLOCK ALL` and it sits at **30000** — the companion's own index — on three
pairs:

```
30000  Internal -> Internal  'Isolated Networks'               BLOCK  ALL   enabled
30000  Internal -> Hotspot   'Isolated Networks'               BLOCK  ALL   enabled
30000  Internal -> Dmz       'Isolated Networks'               BLOCK  ALL   enabled
30000  Internal -> External  'Block Invalid Traffic'           BLOCK  CUSTOM
30001  Hotspot  -> Internal  'Post-Authorization Restrictions' BLOCK  ALL   enabled
30003  Hotspot  -> External  'Block Unauthorized Traffic'      BLOCK  ALL   enabled
```

An `allow` moved to 40000 sits under those. `allow Internal -> Dmz` is an
ordinary line to write — it is the pair ADR-0030's own accepted probe used — and
at 10000 it outranks `Isolated Networks` and works. At 40000 it does not, and the
operator has a policy their file states plainly and their firewall does not have.
That is this ADR's own defect pointing the other way, and shipping it would have
traded one silent firewall for another.

**There is no third value that escapes both**, because the companion and
`Isolated Networks` are at the *same index*. Anything that yields to one yields
to the other. The two goals are not far apart on a number line; they are the same
number.

So each verdict is placed by what it needs in order to mean what it says, and the
asymmetry is the Controller's rather than unifig's: **a companion is always an
`ALLOW`, and always on the reverse pair.** Only a policy that blocks can stop one
being reached, and only a policy that allows is harmed by the generated blocks on
its own pair. An `allow` therefore asks for nothing and keeps the 10000 the
Controller assigns it, exactly as before this change.

**What that decides as a side effect is named rather than dressed up as a
choice.** On one pair, a `block` unifig created now sits below an `allow` unifig
created, so "allow the pair, then block one port of it" written as two policies
does not do what it reads like. That is not unifig ranking the operator's policies
on their merits — the option that would do that is issue #54's option 1, still
declined for ADR-0004's reason. It is what falls out of placing each verdict where
it has to go, and what it replaces is not an order but a **tie**: two policies
unifig created on one pair were both at 10000, and which of them won was whatever
the Controller does with a tie, which nobody has measured either. An operator who
needs a specific order between two of their own policies still has the UI, and had
nothing better before.

## What is not measured, and what unifig does about it

**Nobody has sent a real Controller a create naming an index.** Issue #54 named
that as the one thing this option was blocked on, and it is a question only
hardware settles. This ships under the assumption that the Controller stores what
it is told, and the assumption is named in three places rather than left to look
like a reading: here, on `unifigPolicyIndex`, and on `replay.assignedIndex`,
which is where the stand-in's default encodes it.

What narrows the assumption is that `index` already goes on the wire. An update
carries the whole stored object back, `index` and all, and the object comes back
holding it (ADR-0021) — the probe above did exactly that to toggle `enabled`, and
put the index back unchanged.

**That is weaker evidence than it first reads as, and the weakness is named
rather than leaned on.** An endpoint that ignored the field entirely would answer
that PUT identically, because the value sent was the value already stored. What
it rules out is "a field the write DTO has never heard of" — which is a real
refusal this endpoint family makes, and makes loudly (ADR-0019) — and nothing
more. What a POST does with one is a separate question again: the two v2
collections this project talks to answered the same question about their PUTs
oppositely (ADR-0021, ADR-0024), so neither the version in the path nor the verb
predicts anything here, and inferring a POST's behaviour from a PUT's is the kind
of generalisation ADR-0030 was written after getting wrong.

So **unifig reads the answer back**. `createFirewallPolicy` compares the index in
the object the Controller returned against the one it asked for, and where they
differ apply says so after its summary:

```
The Controller placed 1 firewall policy where it chose rather than where unifig asked:
  "Somewhere below" asked for index 40000 and was placed at 10000
A policy above the companion tier at 30000 outranks the return rule of an allow
unifig made, which drops the reply to traffic that allow permits. Putting it back
is a reordering unifig does not do.
```

The last sentence says what unifig will not do rather than what cannot be done,
because the latter would be false: go-unifi v2.3.0 exposes
`ReorderFirewallPolicies` against a `firewall-policies/batch-reorder` endpoint.
Nobody here has called it, nothing has measured what it does, and a batch reorder
is a statement about every policy on a pair rather than about the one unifig
created — so it is out of scope for this, and naming it here is what stops the
next reader from thinking it was never noticed.

**An absent index is not a zero one.** `index` is `omitempty` on the way in, so a
response that omits it decodes to 0 and is indistinguishable from one that says 0.
Reporting that would put "placed at 0" on the end of every apply against a
Controller whose create response simply does not echo the field — a notice that
fires always, which is a notice nobody reads (ADR-0012). Zero is therefore read as
"declined to say": no Controller has been seen using it, the lowest index in the
86-policy recording being 10000. The shape of a create response is exactly as
unmeasured as the semantics this read-back exists to measure, which is the reason
to be careful with it rather than a reason to guess.

That is the probe, shipped. The first operator to run this on a Controller that
ignores the request finds out at their own site rather than by pinging a host and
getting nothing back — which is exactly how this bug was found, at the cost of a
packet capture.

It is **not an error and does not stop the apply**: the policy was created and
every field the config states landed. What did not land is where it sits in the
rule set, which is a fact about the site rather than about the line the operator
wrote. It is the third thing a Caveat is — an absence with a reason, invisible
unless something says it out loud — arriving one stage later than a Caveat can,
because only the write knows.

And it says nothing where the Controller honoured the request, for ADR-0012's
reason: a notice on every apply is one an operator reads past by the third.

## Considered Options

- **Model `index` as a config field** — rejected. It gives the operator the
  control the Controller has, and makes `unifig.yaml` responsible for a total
  ordering across a site whose other policies it does not name. ADR-0004's
  "unstated is unmanaged" has no comfortable answer for a field whose meaning is
  entirely relative to fields the file omits.
- **Refuse or warn offline, in `validate` and `plan`** — rejected, and it was the
  weakest form on offer rather than a bad one. The inversion is visible in the
  config alone: an `allow` A -> B alongside a `block` B -> A, where the allow's
  destination is not External and so has a companion. No index modelling, no
  Controller round-trip, no rule-set evaluation — the shape of
  `annotateWideGatewayBlock`, inside ADR-0018's declared scope. What defeats it is
  that it fixes nothing. It tells an operator their firewall does not mean what it
  says and leaves the repair in the UI, on every run, forever.
- **Ask for an index below the companion tier** — chosen. It is the only option
  that makes the firewall unifig writes mean what the file says, and the thing it
  costs is one unmeasured assumption, which the read-back converts into a
  measurement at the first site that disagrees.
- **Create as today, then PUT the index** — rejected. It buys certainty that the
  field is accepted, because an update carrying `index` is measured. It costs two
  round-trips per create, a window where the policy is live at 10000, and a second
  failure mode when the PUT is the half that fails. The read-back gets the same
  information for one request.

## Consequences

- **A policy unifig creates that blocks now sits below every policy the Controller
  generates for that pair.** That is the intent, and it has a second effect worth
  naming: the Controller's own specific allows — `Allow mDNS` at 30000 on a custom
  zone's path to the Gateway, `Allow Public DNS`, `Allow WireGuard VPNs` — now
  outrank a block unifig creates, where before they did not. An operator whose
  block was relied on to stop mDNS or a WireGuard tunnel loses that, and gets no
  notice, because unifig reads no rule set and cannot tell that it mattered.
- **An `allow` unifig creates is placed exactly as it was**, which is what keeps
  this change from having a blast radius on the half of the firewall it is not
  about.
- **`annotateWideGatewayBlock`'s note survives it**, and its comment is corrected
  rather than left saying something now false. It used to reason that a policy
  unifig creates lands "above every one of them"; it lands below `Allow mDNS`
  now. The note is still true, because what it is about is what a custom zone's
  leases, name resolution and time ride: the catch-all `Allow All Traffic` at
  2147483647, which a policy at 40000 still outranks.
- **ADR-0018's "does not model `index`" is narrowed, not overturned.** unifig
  asks for one on create. It still reads none, still evaluates no rule set, and
  still makes the Risky mark a statement about what a policy says rather than a
  verdict on what the firewall will do. The hedge in that sentence is unchanged
  and load-bearing for the same reasons.
- **The stand-in gained a knob and an admission.** `replay.assignedIndex` is nil
  by default, which is the assumption; `assignPolicyIndex` is how a test asks for
  the measured behaviour instead — a Controller that assigns 10000 whatever the
  body said. If the probe comes back no, that is a one-field change and the suite
  names every test that was resting on the guess.
- **`docs/COMPATIBILITY.md` is unchanged and the recording is not re-cut.** No new
  endpoint is called and no new field is read; what changed is one field in a body
  unifig already sends.
- **A policy that already exists is not moved, and every site that has this bug
  today still has it.** An update writes back the object the Controller sent with
  the four managed fields overwritten (ADR-0021), so a policy created before this
  keeps its 10000 until it is deleted and recreated. Adding `index` to the fields
  an update owns would fix those sites and would drag back any policy an operator
  had deliberately reordered in the UI, silently, on the next apply — the same
  objection ADR-0004 makes to writing a field the file does not state. The
  narrower fix is the one issue #54's option 3 asked for, and it says *create*.
- **The open question stays open, and is now cheap to close.** Anyone with a
  router can answer it: apply a policy and read the index back. The difference
  from before is that unifig will have told them.
- **The adjacent defect in issue #54 is not fixed here.** `returnRuleField`
  returns early on `requested == want` before it consults `companionHeld`, so a
  policy carrying the flag `true` with no companion by that name reads as "No
  changes" — the plan cannot report a missing companion whenever the flag is
  already correct. Both companions existed in this bug, so it is not what caused
  it, and it is a separate defect.
