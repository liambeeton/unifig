# The plan asks the companion as well as the flag, because the flag being right is not the companion being there

> **Amended by issue #56 on its central inference, not on its decision.** This ADR
> widened the gate and then inferred, in its consequences, that an apply repairs
> the state the widened gate reports — saying plainly that this was "not a write
> anyone has watched". It has now been watched, and it is **wrong**: a true -> true
> PUT regenerates nothing. The companion follows whether the Controller is
> *enforcing* the policy, so the only history reaching the missed state is a policy
> switched off, and `enabled` is not a field an apply moves. The plan is now silent
> there rather than promising a repair it cannot perform (ADR-0035).
>
> The decision stands: the gate still asks both ends, and the table below still
> describes what the plan reports. What the table's third and fourth rows lose is
> their claim to be states a Controller produces — row 4 is unreachable, and row 3
> is reached only by disabling the policy.

ADR-0026 decided that the `return-rule` field is "gated on the flag and rendered
from the companion", and gave the reason for the gate: without it, "the 52
shipped `ALLOW` policies would each look divergent and an exported firewall would
not plan clean". The gate was an early return that compared
`create_allow_respond` and never reached the companion at all.

That leaves two of the four states unreportable, and one of them is the state the
whole feature exists to correct. This amends ADR-0026 on the gate and leaves the
rest of it standing.

## What the gate could and could not see

`returnRuleField` takes three inputs: what the verdict wants, what the policy's
flag requests, and whether the site is holding a `<name> (Return)`. The gate read
two of them.

| flag | companion held | wanted | reported |
| --- | --- | --- | --- |
| false | no | yes | yes — `return-rule: → "X (Return)"` |
| true | yes | yes | correctly silent |
| **true** | **no** | **yes** | **no — missed** |
| **false** | **yes** | **no** | **no — missed** |

_Rows 3 and 4 are marked as missed, and they were. What this ADR did not know is
that neither is a state a Controller produces by the histories it had in mind:
issue #56 measured row 4 unreachable outright, and row 3 reachable only by
switching the policy off — where the plan is now deliberately silent (ADR-0035)._

The third row is the one that matters: an `allow` policy already carrying the
request, with no companion behind it. The plan renders no field, so no field means
no change, so no write goes out, so the companion is never regenerated. **The
state is invisible and it perpetuates itself** — and it is the same firewall as
the row above it, reached by the other of the two histories that lead there.

Which is exactly what ADR-0026 superseded ADR-0025 to stop. A companion is
supposed to follow the config rather than the policy's history; the code kept that
promise only where the *flag* disagreed. Where the flag was already right and the
companion was gone, unifig could neither put it right nor describe it — the state
ADR-0025 existed to describe and ADR-0026 claimed to have fixed.

## The gate's own justification was measured obsolete

The 52 policies the gate was protecting are real and the count has grown. Read
live off the UDR running UniFi Network 10.6.101, 137 policies, 3 September 2026:

```
policies with flag=true and no <name> (Return):     123
  of those, GENERATED (composite _id, unwritable):  123
  of those, STORED (24-hex document handle):          0
```

**Every one of them is a Generated Policy.** Since ADR-0028, export does not write
a Generated Policy into a config file at all, so none of them can appear in a
config, none can be matched, and none reaches `changedPolicyFields`. The noise the
gate was written against can no longer arise from an exported firewall; the
justification predates export dropping them.

Zero *stored* policies on the site are in the missed state, so this was latent
rather than active. It did not cause issue #54 — both companions there exist. It
was found while confirming that #54's third hypothesis was dead, and the finding is
that the plan could not have answered the question either way.

## Considered Options

- **Leave it.** Rejected. A plan is a statement about what apply would do
  (ADR-0014), and here it is silent about something apply would not do and should.
  Latent is not the same as harmless when the state repairs no way but by hand.
- **Render from the flag instead of the companion.** Rejected, and it is the
  defect ADR-0025 caught in review: a plan would promise to remove a companion
  that was never there. ADR-0026's second half is not what was wrong.
- **Widen the gate to `requested != want || companionHeld != want`** — chosen.
  Either end disagreeing is something the plan has to say: the flag because there
  is a write to make, the companion because there is a policy an operator has or
  lacks. Only the pair agreeing is silence.

## Consequences

- **A policy allowing without its companion is now a change whichever history put
  it there.** That is one sentence in two tests —
  `TestPlanSaysTheReturnRuleIsMissingWhenNothingElseDiffers` for the flag-false
  history and `TestPlanSaysTheReturnRuleIsMissingWhenTheRequestIsAlreadyRight` for
  the flag-true one — and they seed the same firewall.
- **The `from == to` check below the gate stays and now carries more traffic.** It
  is what keeps the deliberately silent cases silent: a write worth making with
  nothing to show for it (the request set, no companion by that name, closing) and
  a companion already standing where the config wants one. Widening the gate
  without it would have printed a line for the Controller's own policies, whose
  companions are the twelve `Allow Return Traffic` on reverse pairs.
- **The companion half is not asked of a Generated Policy at all**, which is the
  one thing the issue's sketch of this fix got wrong and the suite caught. Its
  companion is not named `<name> (Return)`: the twelve a migrated router ships are
  `Allow Return Traffic` on reverse pairs, the Controller's scheme for its own
  policies (ADR-0022). So the absence of a `<name> (Return)` beside one is not a
  missing companion, it is the question asked in a name that policy's companion was
  never going to carry — and there is nothing to write to either (ADR-0027), which
  is the same answer from the other side. `returnRuleCompanion` carries the two as
  a pair for that reason: what the site holds, and whether the holding is this
  policy's question. Asked anyway, a file agreeing with a generated allow in every
  word it states would raise a Caveat about a companion nobody is missing — on any
  of the fifty-two `ALLOW` policies among the eighty-six a migrated router ships,
  every one of which carries the flag true with no `<name> (Return)` beside it —
  `TestAPolicyTheControllerGeneratesAndTheFileAgreesWithIsQuiet` is that sentence
  and it failed before the pair existed.
- **Four fixtures were seeding a site no Controller hands back**, and the widened
  gate is what said so: a *stored* allow carrying `create_allow_respond: true` with
  no companion beside it. On a real Controller the companion is generated from that
  very flag, so those seeds now call `seedCompanion` and the plans they assert are
  clean for the reason they claim rather than by the gate not looking.
- **ADR-0026 is amended on its gate and stands on everything else.** The two
  questions it names are still two questions; what changed is that the first one
  is asked of the companion as well as of the flag.
- **That an apply repairs the missed state is inferred, and the inference is
  named here rather than left in the code.** _Measured 4 September 2026 and
  **wrong** — see ADR-0035. A true -> true PUT regenerates nothing; the companion
  follows whether the Controller is enforcing the policy, so the only history that
  reaches the missed state is a policy switched off, and `enabled` is not
  unifig's to change. The plan is now silent there. The rest of this bullet is
  kept as the reasoning that was current when it was written._ ADR-0026 measured the flag *moving* —
  false to true, 87 policies to 88 — and what an update on row 3 re-sends is a flag
  that was already true. The Controller answering that by generating the companion
  is this codebase reading `create_allow_respond` as a statement of what the site
  should hold rather than as an edit, which is what ADR-0026's own wording says and
  is not a write anyone has watched. Row 4 has the mirror of it: an already-false
  flag re-sent is promised to reclaim a standing companion. The stand-in models the
  state rather than the transition, so the suite agrees with the inference and
  cannot test it — one PUT on a `Dmz -> Dmz` throwaway settles both rows, and it is
  the next thing to measure. What is not inferred is the plan: silence claimed a
  completeness it did not have, and a field states a difference an operator can go
  and look at.
- **How a site reaches the missed state is unmeasured.** _Measured now: it is
  `enabled`, and nothing else reaches it (ADR-0035). Deleting a companion on its
  own is refused with the same 404 ADR-0027 measured for `GET` and `PUT`._ ADR-0022 measured the
  Controller reclaiming a companion with its parent, and issue #40's probe measured
  the flag driving the companion in both directions on an update. Nobody has tried
  deleting a companion on its own, or seen the Controller drop one while keeping
  the flag set. The state may be unreachable in practice — which is a reason to
  find out, not a reason to leave the plan claiming a completeness it does not
  have.
- The standing limitation is the standing one: a single household's router, now on
  10.6.101, and `docs/COMPATIBILITY.md` is where that lives.
