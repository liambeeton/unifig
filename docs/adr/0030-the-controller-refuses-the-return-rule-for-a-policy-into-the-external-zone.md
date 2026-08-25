# The Controller refuses the return rule for a policy into the External zone

ADR-0022 settled what unifig sends in `create_allow_respond`, and settled it one
condition short. The rule it wrote down — ask for the companion return rule on an
`allow` and not otherwise — was measured against a `Dmz -> Dmz` probe, and it is
true of that pair. It is not true of every pair, and the half it misses is the
one almost every real config contains: a policy allowing a segment out to the
internet.

The gap was not found by a probe. It was found by an operator applying a
twelve-policy firewall to a live UDR (issue #49), where the apply created three
policies and then stopped:

```
unifig: create firewall-policy "Cyberdelia to internet": Server error (400) for
POST https://<host>/proxy/network/v2/api/site/default/firewall-policies:
Firewall policy create respond traffic not allowed
```

The same message ADR-0022 recorded for a `block`, on a policy whose verdict was
`allow`.

## What the failure looked like, and why it read as something else

Changes run in the order the plan printed them and apply stops at the first
failure (`Plan.Apply`), and policies are ordered by name. So the twelve sorted
with three `Cyberdelia off …` blocking policies ahead of `Cyberdelia to
internet`, and what the operator saw was three creates succeeding and the fourth
refused. Read down the config file instead of down the plan, the failing policy
looks like the ninth of twelve and like nothing in particular. Read in the order
it actually ran, it is the **first `allow` the apply came to** — and no `allow`
policy had ever been created on that router by unifig at all.

The half-applied firewall is the consequence worth naming. Nothing was rolled
back, three blocking policies were live, the allow that was supposed to let that
segment reach the internet was not, and every policy sorting after it was
untouched.

## What was measured

Three probes against the live migrated UDR on Network 10.5.67, 25 August 2026,
from a baseline of 123 policies (issue #49). The probe pair is `Dmz -> …`
throughout wherever it can be: `Dmz` holds no networks, which is why ADR-0022 and
ADR-0029 both used it. Each probe deleted what it created and the count returned
to 123.

**The destination is what is refused.**

```
POST  Dmz -> External, ALLOW, create_allow_respond: true
400   Firewall policy create respond traffic not allowed
```

Reached from a zone with no networks, no history and no relation to the
operator's config, so what the first failure could not separate — the zone having
just been created, the zone holding a network, anything about that site's
`Cyberdelia` at all — is separated here. It is the destination.

**The Controller takes the same policy without the request.**

```
POST  Dmz -> External, ALLOW, create_allow_respond: false
201   created, index 10000; site 123 -> 124
```

One policy, not two: no companion was generated. This is the measurement the fix
rests on. Without it, "stop asking" would be a guess that the endpoint objects to
the request rather than to the policy, and a fix built on it could as easily have
turned one 400 into another.

**It is not the reverse pair being occupied.**

```
POST  Internal -> Dmz, ALLOW, create_allow_respond: true
201   created; site 123 -> 125
```

Two policies: the parent and its companion. `Dmz -> Internal` already carried a
`RESPOND_ONLY` policy of the Controller's own, so the Controller generated a
second return rule onto a pair that already had one.

That last probe is here because the hypothesis it refutes fitted everything known
before it was run. Creating the `Cyberdelia` zone makes the Controller compute
policies for every pair the zone is in, `External -> Cyberdelia` among them, and
the site was indeed holding an `Allow Return Traffic` on exactly the reverse pair
of the policy that failed. "The Controller will not generate a second companion
where one already sits" explained the failure, explained ADR-0022's success —
`Dmz -> Dmz` has no return rule on its reverse — and was wrong. It survived
argument and died to one request, which is the whole reason ADR-0019's discipline
exists.

**What none of this measures** is why. That a gateway keeps track of the return
path for traffic it is masquerading, so a companion policy would have nothing to
do, is a reading of the Controller's behaviour and not a measurement of it.
Nothing here depends on it being right.

## Considered Options

- **Keep ADR-0022's rule and let the operator work around it.** The workaround is
  to delete every `→ internet` allow from the config, which is to say: unifig
  cannot express the most ordinary firewall there is.
- **Stop asking for the companion entirely.** This is the state issue #36 was
  filed about, and it would give up the companion on every pair that does take
  one to fix the pairs that do not.
- **Suppress the request where the reverse pair already holds a `RESPOND_ONLY`
  policy.** The hypothesis the third probe refuted. It would have fixed this
  operator's apply — every `External -> X` pair on their router carries a return
  rule — while being wrong about the rule, and wrong in a direction that silently
  drops the companion on pairs like `Dmz -> Gateway`. Fixing the symptom by
  accident is worse than not fixing it, because nothing afterwards would say so.
- **Ask for the companion on an allow whose destination is not External** —
  chosen. It is the rule the two accepted probes bound from either side: refused
  with the request, accepted without it, and still generating companions
  everywhere else.

## Consequences

- **`asksForReturnRule` is the one place that decides**, and the create, the
  update and the plan all go through it. They were three expressions of
  `opensAPath(desired.Action)` before, in `newFirewallPolicy`,
  `setReturnRuleRequest` and `returnRuleField`, and a second condition arriving
  is exactly the event that would have left them disagreeing. It is the reason
  `options()` exists for `--prune`, applied to a rule instead of a flag.
- **A policy's ends decide what unifig sends, so the write path needs the zone
  facts.** `newFirewallPolicy` took the config entry to learn its verdict
  (ADR-0022); it takes `zoneFacts` now to learn what its destination is. The same
  threading reaches the update through `mergeIntoStoredPolicy` and
  `overwriteManaged`.
- **The External zone is found by the Controller's own `zone_key`**, beside the
  gateway and internal zones already read there (ADR-0018, ADR-0020). Not by the
  name "External", for the reason those two are not: a firmware presenting it
  under another name would silently turn the rule off, and silence is the failure
  this cannot afford — it would be back to a refused apply with no explanation.
- **The plan stops promising a companion it will not get.** `returnRuleNote` and
  `returnRuleField` ask the same predicate, so a create into External carries no
  note about reply traffic and an update into External proposes no `return-rule`
  field. ADR-0014's standard, in a new place: a plan is a statement about what
  will happen.
- **Unknown zone facts mean unifig still asks.** `intoExternal` is false when the
  Controller could not be asked, which leaves the pre-existing behaviour in place
  rather than quietly suppressing the request everywhere on a failed read. The
  loud failure names the policy; the quiet one would leave an allow policy
  without the companion its neighbours have and say nothing.
- **The stand-in refuses what hardware refuses.** `refusedByPolicyWrite` reads the
  destination's `zone_key` as well as the verdict, and the four tests added here
  fail against the old rule with the operator's own error message. That is the
  cover ADR-0019 asks for, and it is worth saying plainly that it did not exist
  before: the suite was green throughout, on both the old rule and the operator's
  exact config, because the stand-in modelled the refusal as narrowly as the ADR
  that described it. A stand-in built from a measurement inherits that
  measurement's blind spots.
- **Nine existing tests moved off `Internal -> External`.** Every test whose
  subject is the companion had been written on that pair, which can no longer
  witness a companion at all. They state `Internal -> Dmz` now — the pair the
  third probe was accepted on.
- **`docs/COMPATIBILITY.md` is unchanged and the recording is not re-cut.** The
  three probes were reads and writes against one router, not a re-recording; the
  zone keys the fix depends on were already in `e2e/testdata/udr/firewallzone.json`.
- The standing limitation is the standing one: a single household's router on
  10.5.67. What is new is a reminder about the shape of that limit rather than
  its size — ADR-0022's rule was right about the pair it was measured on and
  wrong in general, and a probe pair chosen because it is safe (`Dmz`, holding
  nothing) is by construction not a pair a real config uses.
