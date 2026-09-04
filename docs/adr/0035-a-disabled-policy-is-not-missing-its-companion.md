# A disabled policy is not missing its companion, because the companion follows what the Controller is enforcing

ADR-0034 widened the plan's `return-rule` gate to ask the companion as well as
the flag, and named an inference in its consequences rather than leaving it in
the code: that an update re-sending an already-true `create_allow_respond`
repairs a missing companion. It said plainly that this was "not a write anyone
has watched", and that one PUT on a `Dmz` -> `Dmz` throwaway would settle it.

Issue #56 is that probe. It was run on the live migrated UDR on 4 September 2026,
Network 10.6.101, on a throwaway `Dmz` -> `Dmz` policy — the pair ADR-0022 and
ADR-0026 used, chosen again because it holds no networks and nothing rides it.

**The inference is wrong, and the reason it is wrong is a field nobody had
looked at.**

## What was measured

The probe's first stage asked a question ADR-0034 had left open: how a site
reaches "flag true, no companion" at all.

**The companion cannot be deleted on its own.** Its `_id` is the composite ADR-0027
describes, and `DELETE` answers it exactly as `GET` and `PUT` do:

```
DELETE .../firewall-policies/6a8dbef0…627f6a8dbef0…627f30000
  404 {"code":"api.err.FirewallPolicyNotFound"}
```

That is the third verb, and it extends ADR-0027 rather than qualifying it: a
Generated Policy has no handle for any of them.

**`enabled` is what reaches the state.** With the flag held true throughout, and
every count read off the API:

```
create allow, flag true          137 -> 139 policies, companion present
disable the parent, flag true    139 -> 138 policies, companion GONE
true -> true PUT, still disabled 138 -> 138 policies, companion STILL GONE
re-enable the parent, flag true  138 -> 139 policies, companion BACK
```

So the companion is a **projection of the parent's current state**, recomputed on
every write:

> a companion exists exactly where the policy is `enabled`, allows, carries the
> request, and is not destined for the External Zone

and the third line is the one this ADR turns on. A true -> true PUT repairs
nothing, because the flag was never what was wrong.

**Row 4 does not occur.** Setting the flag false on an enabled allow took the
companion in the same write (139 -> 138), and there is no state in which the flag
is false and the companion still stands. The mirror ADR-0034 promised has nothing
to be a mirror of.

**Row 3 does not occur either, with the policy enabled.** Every reading says the
companion is generated deterministically from the flag while the policy is on. The
only history that reaches "flag true, no companion" is a policy switched off —
which an operator does in one click in the UI.

## Why that made shipped code dishonest

`enabled` is not a field an apply can move. unifig names it exactly once, on a
create, where `newFirewallPolicy` sends `Enabled: true` — it is one of the six
booleans go-unifi puts on the wire regardless, and the one unifig names on
purpose. What it is not is a line of `config.FirewallPolicy`, or one of the four
values `overwriteManaged` writes: an update merges into the object the Controller
sent (ADR-0021), so a policy an operator switched off in the UI stays off across
every apply. That is ADR-0004's ordinary rule, and it is the right outcome — a
config file that does not mention `enabled` must not switch a firewall rule back
on.

So on a disabled allow that a config file names, the shipped gate reported:

```
~ firewall-policy "Switched Off And Answering Nothing"
    return-rule: (none) -> "Switched Off And Answering Nothing (Return)"
```

and apply could not deliver it. Measured end to end on the router: the PUT
returned `200`, the count stayed at 138, and `unifig plan` came back with the
same line and exit 2. **A plan still dirty after an apply is the tell**, and issue
#56 named it in advance as the reading that would decide this.

That is ADR-0034's second branch, and it is the branch that says code already on
`main` is not honest: a promise of a repair that does not happen is worse than the
silence it replaced, because silence at least did not lie about what apply would
do.

## Considered Options

- **Leave it and describe the state in a Caveat.** Rejected, and it is what issue
  #56 expected to be forced into. A Caveat is for a change unifig cannot make to
  an object it otherwise manages (ADR-0027). This is not that: nothing is wrong
  with the policy, the Controller is behaving correctly, and there is no repair
  being withheld. A Caveat here would report the Controller working as designed.
- **Model `enabled` and switch the policy on to get the companion back.**
  Rejected, and firmly. The operator disabled the policy on purpose; a config file
  that does not mention `enabled` must not turn a firewall rule back on, which is
  precisely the failure ADR-0004's rule exists to prevent. It also makes every
  disabled policy on a site a pending change on first plan. Note that unifig does
  set `enabled` on a create, so a policy unifig makes is enabled and has its
  companion — the asymmetry is deliberate and is the same one ADR-0021 draws
  everywhere else: a create states a starting shape, an update merges.
- **Ask the companion half only where the Controller would be generating one** —
  chosen. The absence of a companion beside a disabled policy is not a companion
  missing; it is the Controller's own rule, observed. unifig says nothing about
  `enabled`, so it says nothing about what `enabled` decides.

## What changed

`returnRuleCompanion` grows a third field, `enabled`, filled by `companionOf`
from the live policy — alongside `named`, and for the same kind of reason. `named`
already asks whether the site's answer is evidence about *this* policy's
companion; `enabled` asks whether there is an answer to be had at all. Both are
constructed together for the reason the constructor already gives: halves of one
rule filled in separately are halves that come to disagree.

`returnRuleField` then separates two questions that had been one value:

- `want` — what the config's verdict asks the Controller for. **Unchanged**, and
  still what the flag is compared against, because the request is unifig's to own
  whatever the Controller does with it (ADR-0026). It still goes out true on a
  disabled allow, so the companion appears the moment the policy is switched on.
- `holds` — `want && companion.enabled`, what the site will actually be holding
  once the request is right. This is what the companion is compared against and
  what the `to` end of the field is rendered from.

Gating only the comparison would have missed the other road to the same false
promise: a disabled allow whose flag is *also* false has a write worth making, so
the guard does not fire, and the field would still have named a companion on its
`to` end. Rendering from `holds` collapses that into the `from == to` case
ADR-0034 already relies on for a closing verdict — a write worth making with
nothing to show for it — and the plan stays silent.

## Consequences

- **The plan is quiet about a disabled policy's companion**, in both flag states.
  `TestPlanIsQuietAboutTheCompanionOfADisabledPolicy` and
  `TestPlanDoesNotPromiseACompanionForADisabledPolicy` are those two sentences,
  and both failed before the change with the exact line the router produced.
- **ADR-0034's inference is withdrawn, not amended.** A true -> true PUT does not
  regenerate a missing companion. Nothing else in ADR-0034 moves: the gate still
  asks both ends, and the four-row table still describes what the plan reports.
  What the table's third and fourth rows lose is their claim to be states a
  Controller produces.
- **The two tests for rows 3 and 4 now guard states measured not to occur.**
  `TestPlanSaysTheReturnRuleIsMissingWhenTheRequestIsAlreadyRight` and
  `TestPlanSaysTheReturnRuleGoesWhenTheRequestIsAlreadyFalse` seed an enabled
  policy, so both still pass and neither was touched. They are kept deliberately:
  the measurement is one firmware on one router, and a plan that behaves sensibly
  on a state this firmware does not produce costs nothing. What they may no longer
  be read as is evidence that apply would repair such a state — it would not, and
  the stand-in models the state rather than the transition, so it cannot say
  otherwise.
- **The stand-in models `enabled` too, and had to.** `reconcileCompanion` read
  the request and the verdict and would have generated a companion for a policy
  the Controller is not enforcing — a firewall no router hands back, in exactly
  the place this change is about, so the suite would have agreed with the bug it
  was written to catch. It is the same class of fixture defect ADR-0034 found in
  four seeds, caught this time by having measured the rule first.
  `TestADisabledPolicysRequestIsStillWrittenByAnUpdateThatHappensAnyway` is what
  holds it: the request goes out correct on an update that happens for another
  reason, the policy is *not* switched back on, and no companion appears.
- **The flag write on a disabled allow is silent**, which is the pre-existing
  design rather than a new gap: a flag that differs with no companion to show for
  it renders nothing, exactly as it does for a closing verdict. If that policy
  differs in nothing else, no update is planned and the flag stays as it is until
  something else about the policy changes. The consequence is bounded — an
  operator enabling such a policy gets no companion until the next apply that
  touches it — and it is the same trade ADR-0034 made knowingly.
- **`pairsCarryingCompanions` has the same false premise and is not fixed here.**
  Its config half predicts a companion from `asksForReturnRule` alone, so a
  disabled allow in the file makes ADR-0033 reorder a blocking policy below a
  companion tier that does not exist. It is a real defect, it is narrower — it
  needs the live policy's `enabled` at a call site that currently reads only the
  config — and it is filed separately rather than folded into a change about what
  the plan says.
- **ADR-0027 gains its third verb.** `DELETE` on a Generated Policy's composite
  `_id` answers `404 api.err.FirewallPolicyNotFound`, the same as `GET` and `PUT`.
  The companion cannot be removed on its own by anyone, which is also why the only
  route to row 3 was the one this ADR is about.
- The standing limitation is the standing one: a single household's router, now on
  10.6.101, and `docs/COMPATIBILITY.md` is where that lives.
