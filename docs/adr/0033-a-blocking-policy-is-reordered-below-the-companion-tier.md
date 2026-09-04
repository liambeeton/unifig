# A blocking policy is reordered below the companion tier, because `index` is not writable

unifig was creating firewall policies that outranked the companion return rules of
its own allows, and there was no config an operator could write to avoid it. The
fix is a second request to a second endpoint: `index` is the Controller's to
assign, and `batch-reorder` is the only way a client can say where a policy sits.

## What was measured — the bug

Live UDR running UniFi Network 10.6.101, 137 policies, 3 September 2026 (issue
#54). The config stated the ordinary pair of intentions — trusted reaches IoT, IoT
does not reach trusted:

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

The companion was created exactly as ADR-0022 and ADR-0026 measured, and is never
reached: the operator's own block sits 20000 above it. A second instance on the
same site, unnoticed until it was looked for: `Management to Ellingson (Return)`
at 30000 under `Ellingson off management` at 10000.

**Confirmed at packet level in the same session**, one variable moved — `enabled`
on the reverse block, restored afterwards:

```
block ENABLED   (baseline)   ping 10.15.7.202 …  100% loss
                             18 TCP connects across 7 hosts … all TIMEOUT, zero RST
block DISABLED  t=10s        REPLY  (sweep of five hosts, all REPLY)
block RE-ENABLED             no reply
```

Two details worth carrying: the dataplane takes about ten seconds to reload, so a
three-second settle reads as a clean refutation of the whole hypothesis; and the
same site holds a natural control in `Gibson to management`, an allow with a
companion and **no reverse block**, which answered throughout.

## What was measured — the mechanism

The first attempt at this named `index` in the create body, on the reasoning that
40000 sits below the companion tier and clear of the generated band. **That
reasoning was right about the number and wrong about how to ask for it.** Probed
on the same router on 4 September 2026, on a throwaway `Dmz -> Dmz` policy — a
pair holding no networks, deleted afterwards, site returned to 137 policies:

```
POST  index: 40000  ->  201 Created, stored at 10000
PUT   index: 40000  ->  200 OK,      stored at 10000
```

The Controller takes the field on both verbs and ignores it. `index` is
server-assigned; the *position* is what a client may ask for, through a different
endpoint entirely.

**`PUT .../firewall-policies/batch-reorder` is that endpoint.** It takes a pair
and two lists of stored policy ids, and the Controller assigns the indices:

| named in | Controller assigns |
| --- | --- |
| `after_predefined_ids` | **40000** |
| `before_predefined_ids` | **10000**, then 10001, … |

So `before` is where a create lands on its own, `after` is below the tier the
companions are generated into — and 40000 is the Controller's own number for it
rather than one unifig picked. The arithmetic in the abandoned draft was a correct
prediction of a value unifig turned out not to choose.

**An empty list must be `[]` and never `null`.** Neither field is `omitempty` in
go-unifi's DTO, so a nil slice marshals to `null` — and the first real apply
against the router answered `500` with a Tomcat error page and no message in it.
That is the commonest shape there is rather than an edge: a pair with one stored
policy on it puts one list at nothing, which is exactly the two pairs issue #54
was about. Nothing was applied, and the site was unchanged at 137 policies.

**It refuses a partial list.** Naming one of two stored policies on a pair
answered `400 api.err.ShouldIncludeFirewallPolicyInBatchUpdate`; naming both
answered 200 and placed them as asked. So unifig cannot move one policy without
saying where every stored policy on that pair goes.

**And two policies on one pair were never tied.** The second create came back at
`10001`. The abandoned draft claimed both sat at 10000 and that the fix was
therefore replacing a tie with an order. It was replacing an order with a
different order, and the claim is gone rather than corrected in place, because it
was load-bearing for a decision that no longer exists.

## The rule

**A policy whose verdict closes a path is moved below the companion tier, on a
pair that carries a Return Rule, and nothing else is moved.** Three clauses, each
ruling out a way of getting it wrong.

*A verdict that closes a path*, because a companion is always an `ALLOW` and
always on the reverse pair, so only a policy that blocks can stop one being
reached. An `allow` is left exactly where the Controller puts it — and that is not
symmetry for its own sake. The generated tier is not all return rules:

```
30000  Internal -> Internal  'Isolated Networks'               BLOCK  ALL   enabled
30000  Internal -> Hotspot   'Isolated Networks'               BLOCK  ALL   enabled
30000  Internal -> Dmz       'Isolated Networks'               BLOCK  ALL   enabled
30001  Hotspot  -> Internal  'Post-Authorization Restrictions' BLOCK  ALL   enabled
30003  Hotspot  -> External  'Block Unauthorized Traffic'      BLOCK  ALL   enabled
```

An `allow` moved below those is a policy the file states plainly and the firewall
does not have — this ADR's own defect pointing the other way. There is no third
position that escapes both, because the companion and `Isolated Networks` are at
the *same index*: anything yielding to one yields to the other. Each verdict is
placed by what it needs in order to mean what it says.

*On a pair that carries a Return Rule*, because a block only outranks a companion
where there is one. A block on `Internal -> External` has no reply traffic to
strand. Without this clause a brownfield `export` produced a config whose first
`plan` proposed moving every blocking policy on the site — a great deal of change
to justify with a bug that reaches almost none of them, and it broke the
export-then-plan-clean property three tests exist to hold. The pairs are read from
two places, because a companion can arrive from two: a live policy that is a
Return Rule, and an allow *this config states on the reverse pair*, which is where
the Controller will put one (ADR-0022). Reading the config rather than waiting for
the write is what makes one apply enough — a block and the allow whose companion
it would outrank can be created in either order, and the order is whatever their
names sorted to.

*And nothing else*, which is what a reorder requiring the whole list makes into a
decision. A stored policy the config does not name **keeps the side it is on**,
and the order within each list is the order the Controller already has. That is
ADR-0004 reaching a place ADR-0004 did not anticipate: an endpoint that will only
take the complete list is not a reason to start managing the rest of it. unifig
has nothing to say about the relative order of two policies the operator wrote —
issue #54's option 1, still declined.

## It converges, and that is not a detail

Placement is a **field of the policy's change**, like the return-rule request:
`placement: before the return rules -> after the return rules`. It can be the only
thing differing, which is exactly the case issue #54 was reported from — `plan`
said "No changes" about a firewall that was dropping replies.

The abandoned draft moved policies only on create. **That would have fixed
nobody's firewall, including the one this was reported from**, whose fourteen
stored policies already existed at 10000. A fix for a bug found in the field has
to reach the field it was found in.

A reorder that fails is returned as an error rather than a notice. The policy
exists and is misplaced; apply stops and says what did and did not happen; the
next plan reads the live index, sees the wrong side, and proposes the placement
again. That is the recovery path, and it is the same one ADR-0001 gives everything
else: no rollback, run again.

## Considered Options

- **Model `index` as a config field** — rejected before it was measurable, and
  moot now: the field is not writable, so a config stating it would state
  something no request can deliver. The original objection stands on its own — it
  makes `unifig.yaml` responsible for a total ordering across a site whose other
  policies it does not name, and ADR-0004 has no answer for a field whose meaning
  is entirely relative to fields the file omits.
- **Refuse or warn offline, in `validate` and `plan`** — rejected. The inversion
  is visible in the config alone and needs no round-trip, which is the shape of
  `annotateWideGatewayBlock` and inside ADR-0018's declared scope. What defeats it
  is that it fixes nothing: it tells an operator their firewall does not mean what
  it says and leaves the repair in the UI, on every run, forever.
- **Name the index on create** — attempted, shipped, and refuted by the probe
  above. It is recorded here rather than quietly dropped, because the reasoning
  that produced 40000 was sound and is reused, and because "the field is accepted"
  turned out to be a fact about parsing rather than about storing.
- **Reorder on create only** — rejected on the convergence argument above.
- **Send the reorder for every managed blocking policy** — rejected on the
  export-plans-clean evidence above.

## Consequences

- **unifig reads `index`.** ADR-0018 said it did not, and that sentence is
  narrowed rather than overturned: this reads the field to know which side of one
  boundary a policy is on. It still evaluates no rule set and still decides
  nothing from the indices of two policies relative to each other, so the Risky
  mark stays a statement about what a policy says rather than a verdict on what
  the firewall will do.
- **A second endpoint and a second write.** A create or update of a blocking
  policy on a companion-carrying pair now costs a `GET` of the policy collection
  and a `PUT` to `batch-reorder`. The `GET` is the price of an endpoint that will
  not take a partial list, and it happens at write time rather than plan time for
  the reason every write here does: a policy created earlier in the same apply is
  one the pair now holds.
- **A Generated Policy is exempt, and needed saying.** Its index is 30000 or
  2147483647 by construction, so without the exemption every generated policy the
  config agreed with read as misplaced — which is how three passing tests started
  failing. It has no document handle for the reorder lists to name it by, and it
  is what those lists are named *relative to* (ADR-0027, ADR-0028).
- **The stand-in models the refusals as well as the success.** `assignsIndex`
  overrules the body on create; the reorder answers
  `api.err.ShouldIncludeFirewallPolicyInBatchUpdate` to a partial list, and 500 to
  a `null` list. A stand-in that stored what it was handed would have passed the
  version of unifig this ADR is a correction of — which is not hypothetical,
  because it did (ADR-0019). The `null` case is the same lesson a second time and
  the sharper one: the suite was 263 green against a payload the router would not
  take, because the stand-in decoded `null` and `[]` into the same nil slice. It
  decodes them into pointers now.
- **The first apply against real hardware found a defect the whole suite could
  not.** That is worth stating plainly rather than filing under testing: every
  measurement in this ADR came from a router, and so did the one bug that survived
  to the end.
- **`docs/COMPATIBILITY.md` is unchanged and the recording is not re-cut**, but a
  new endpoint is called for the first time and the matrix cannot speak for it: no
  container has a zone-based firewall (ADR-0013), so the evidence for
  `batch-reorder` is one router on 10.6.101 plus the stand-in built from it.
- **The measurements cost two write sessions on live hardware.** Everything
  asserted here about POST, PUT, `batch-reorder` and the partial-list refusal was
  taken on a throwaway `Dmz -> Dmz` policy, one variable at a time, with the site
  returned to its baseline count each time.
- **The adjacent defect in issue #54 is still not fixed here.** `returnRuleField`
  returns early on `requested == want` before it consults `companionHeld`, so a
  policy carrying the flag `true` with no companion by that name reads as "No
  changes". Both companions existed in this bug, so it is not what caused it.
