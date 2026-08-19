# unifig owns the return-rule request, because the flag drives the companion on an update too

ADR-0025 decided that unifig would not own `create_allow_respond` on the update
path, and would have the plan describe the resulting inconsistency instead. It
said why in one sentence: setting the flag is "a write nobody has watched a
Controller answer".

Somebody has now. This supersedes that decision.

## What was measured

Against the live migrated UDR on Network 10.5.67, 19 August 2026, as issue #40's
own probe. The pair throughout is `Dmz` -> `Dmz`, which holds no networks, so
nothing rides it — ADR-0022's probe pair, chosen again for the same reason.

The site's baseline is **86 policies**, all `predefined`, 52 `ALLOW` and 34
`BLOCK`, and **every one of them carries `create_allow_respond: true`**. It holds
twelve companions, all named `Allow Return Traffic`, sitting on reverse pairs.
There is no `<name> (Return)` anywhere on it.

Each step below moved **one variable**, with the verdict held at `ALLOW`
throughout — which is exactly what ADR-0022's reading could not do, because the
request it had changed the flag and the verdict in the same body:

| step | request | policies | companion |
| --- | --- | --- | --- |
| create it allowing, flag true | POST | 86 -> **88** | present |
| **clear the flag, verdict untouched** | PUT | 88 -> **87** | **gone** |
| **set the flag, verdict untouched** | PUT | 87 -> **88** | **back** |
| delete the policy | DELETE | 88 -> **86** | reclaimed, id for id |

**Clearing the flag removes the companion, and setting it generates one.** Both
on an update, both with nothing else moving. That closes issue #40's second
acceptance box, which ADR-0022 had left at "partly, and not cleanly enough to
tick", and it answers the question ADR-0025 was blocked on.

The companion's `_id` is worth recording: `<source zone id><destination zone
id>30000`, the two Dmz ids concatenated with the index. That is the composite
shape issue #41 describes, observed here on a generated policy whose parent was
custom — so the `_id` scheme belongs to generated policies rather than to
shipped ones.

## Considered Options

- **Keep ADR-0025's decision**: describe the inconsistency, do not fix it.
  Rejected — its only argument was the missing reading, and the reading arrived.
  Issue #40's first option opens "Own the flag on the update path", and its
  stated cost was that "clearing `create_allow_respond` may or may not remove an
  existing companion — unmeasured". It is measured; it does.
- **Re-create rather than update on a verdict change.** Still rejected, and now
  clearly unnecessary: the update does the whole job in one request, keeps the
  policy's `_id`, and keeps everything an operator narrowed in the UI
  (ADR-0021).
- **Own the flag on the update path** — chosen. `setReturnRuleRequest` writes it
  to match the verdict the config states, which is the create's rule applied to
  the object the merge puts back.

## Consequences

- **A policy's companion follows the config rather than its history.** This is
  the whole of issue #40. Two operators applying the same file now get the same
  firewall, and `TestTheSameConfigGivesTheSameFirewallWhateverThePolicysHistory`
  is that sentence as a test: one config, applied to a policy created allowing
  and to a policy created blocking and then allowed, both ending with the
  companion.
- **`create_allow_respond` is the fifth field unifig owns on a policy**, and the
  first that is not a line of the config — it is the verdict, restated as the
  request the Controller acts on. `overwriteManagedPolicy` and
  `storedPolicy.overwriteManaged` both state it, which is the two-halves shape
  ADR-0023 describes and its known cost.
- **The plan carries a `return-rule` field, not a note.** ADR-0025 hung its
  sentences off `action`, which was the only field that could be moving. Under
  ownership the return rule can differ on its own — a policy left at `allow`
  with no companion by a unifig older than #36 has every modelled field agreeing
  — and a note cannot describe a change that no other field is carrying. This is
  the last part of issue #40's complaint that "`plan` is clean in both cases":
  it is not clean any more, and applying it converges the two.
- **The field is gated on the flag and rendered from the companion**, which are
  two different questions and were one bug away from being confused. The flag
  decides whether there is a write to make; the presence of a `<name> (Return)`
  decides what the plan may claim. Without the first, the 52 shipped `ALLOW`
  policies would each look divergent and an exported firewall would not plan
  clean. Without the second, a plan would promise to remove a companion that was
  never there — the defect ADR-0025 caught in review, preserved here because the
  reason for it has not changed.
- **The stand-in generates and reclaims companions.** It has to, or an apply
  that asks for one is not idempotent in the suite: the next plan would still
  see a companion missing, which is a real failure the old stand-in would have
  hidden. It models exactly the four readings above and nothing more.
- **ADR-0025 is superseded on its decision and stands on its readings.** What it
  found about the flag not being evidence of a companion is still true and is
  still load-bearing — it is why the `return-rule` field renders the way it
  does.
- **What a companion does to traffic is still unmeasured**, and it is issue #40's
  one remaining box. It mattered most while the two paths disagreed, because it
  decided whether that was cosmetic or a hole; with the paths agreeing it decides
  only how much the whole feature is worth. Nobody has sent a packet through one.
- The standing limitation is the standing one: a single household's router on
  10.5.67, and `docs/COMPATIBILITY.md` is where that lives.
