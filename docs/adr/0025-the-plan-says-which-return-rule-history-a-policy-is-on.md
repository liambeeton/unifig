# The plan says which return-rule history a policy is on, because unifig does not own the request

A Firewall Policy created allowing has a second policy beside it — the companion
return rule the Controller generates, named `<name> (Return)` — and one created
blocking and later allowed does not. Two operators applying the same config file
get different firewalls, depending only on what their policy used to be. Nothing
in the config expresses the difference, and until now nothing in the plan did
either.

That is ADR-0022's own last open question, filed as issue #40. Since then #37's
fix moved a second silence into the same place. Before it, an `allow` -> `block`
update failed loudly — a 400, apply stopped, nothing written. After it the update
succeeds, and the Controller drops the companion along with the request for it:
the site went 88 policies to 87 in exactly that step. Approving a one-word
verdict change was deleting a second policy, and the plan named one.

## What is known, and what is not

Everything here was measured on the live migrated UDR on Network 10.5.67, on 18
and 19 August 2026, and is recorded in ADR-0022. Nothing new was measured for
this decision; what it does is choose what to do with what there is.

**Known.** The flag is a request made at creation, not a property of the policy —
which is why thirty-four policies that block carry it `true`. A create asking for
it on an allow makes the companion, and a create asking for it on anything else
is refused outright, on the create and on the update alike. Deleting the parent
reclaims the companion. An update that closes a path while clearing the request
loses the companion: 88 policies to 87, the `(Return)` the one missing.

**Not known**, and both readings need the router:

- What a companion does to traffic. Nobody has sent a packet through one. That a
  `RESPOND_ONLY` policy named "Return" carries the reply is inference from its
  shape and its name, and it decides whether any of this is cosmetic or a hole.
- Whether clearing the request on its own removes an existing companion. The one
  reading available changed two things in one request — the flag was cleared and
  the verdict closed — so by the one-variable discipline ADR-0019 arrived at, it
  is not a measurement of the flag.

The second of those is what the obvious fix would depend on. Setting
`create_allow_respond` when the verdict opens a path — so the companion follows
the config rather than the history — is a write nobody has watched the Controller
answer: it may generate the companion, or it may store a flag that does nothing,
and the difference is invisible from unifig's side. Its mirror, whether clearing
the flag takes a companion away, is unmeasured for the same reason.

## Considered Options

- **Own the flag on the update path**: set it when the verdict opens a path,
  clear it when it closes. Makes the config the whole truth, and is the option
  issue #40 leans toward once it can be checked. Rejected *for now*: it turns an
  unmeasured Controller behaviour into unifig's stated promise. A plan saying
  "the Controller will also create X" when the request silently does nothing is a
  plan that lies, which is worse than the plan that says nothing — and the
  reading that would settle it is one request on a throwaway policy, on hardware
  nobody has in front of them today.
- **Re-create rather than update on a verdict change.** Honest about what the
  Controller does with the request, and out of proportion: a delete-and-create is
  a much bigger thing to hand an operator for a one-word edit, the policy's `_id`
  changes, and everything an operator narrowed in the UI goes with the old
  object — which is the whole of what ADR-0021 exists to protect.
- **Leave it.** Defensible only while nobody has to act on it. The plan is clean
  in both histories today, so an operator cannot tell them apart even after the
  fact.
- **Say in the plan which history the policy is on** — chosen. It is what unifig
  can state truthfully with the readings that exist, it needs no hardware, and it
  is ADR-0014's standard applied where the standard already applies: a plan is a
  statement about what will happen.

## Consequences

- **The flag is not a companion, and asking it alone was a defect this ADR
  nearly shipped.** The first draft read `create_allow_respond: true` as proof
  that a `<name> (Return)` exists. It is not, by this ADR's own quoted line: the
  flag is the request made at creation. The recording settles it — fifty-two of
  the eighty-three policies a migrated router ships are `ALLOW` carrying the flag
  true, and **not one** has a `<name> (Return)` beside it. The twelve companions
  such a router holds are called `Allow Return Traffic` and sit on reverse pairs,
  which is the Controller's scheme for its own policies rather than the one it
  uses for a policy unifig created. So the plan asks the site as well as the
  flag, and promises a deletion only where a policy by that name is actually
  there. Reading the flag alone would have promised to delete fifty-two policies
  that do not exist.
- **The companion is matched by name, not by `origin_id`.** `origin_id` is what
  really links one to its parent, and `go-unifi` v2.3.0 does not model it, so the
  struct a plan reads cannot see it (ADR-0021). The name is what the plan has to
  print either way, so the claim made is exactly as strong as the sentence it
  supports: a policy by that name is here.
- **`returnRuleUpdateNote` is the update path's counterpart of
  `returnRuleNote`**, hung off the verdict for the same reason and reading the
  live policy's `create_allow_respond` — the only record of whether the
  Controller ever made a companion. Neither `config.FirewallPolicy` projection
  carries it, which is why the note is attached in `updateFirewallPolicy` rather
  than computed inside `changedPolicyFields`: the diff stays a function of the
  two projections, and the consequence that is not in either of them is annotated
  onto it.
- **Three of the four states say something**, and each is a row of issue #40's
  table:

  | verdict | request standing | `<name> (Return)` on the site | the plan says |
  | --- | --- | --- | --- |
  | allow -> block, reject | yes | yes | the Controller will also delete it |
  | allow -> block, reject | either | no | nothing — there is no companion to lose |
  | block, reject -> allow | no | — | this update makes no companion, and a policy created allowing has one |
  | block, reject -> allow | yes | — | the request goes back untouched, and what the Controller does with it is not measured |

  **`reject` is a reading across in the first row, and the only one here.**
  `block` was sent and watched; `reject` never reached the wire, in this probe or
  in ADR-0022's. It is the same body differing in a verdict the Controller has
  treated identically everywhere anyone has looked, and the alternative is a plan
  that goes quiet on one of the two ways an operator closes a path. It is
  recorded rather than absorbed, on the standard `refusedByPolicyWrite`'s own
  comment keeps: what was measured and what is inferred are not the same claim.

- **The plan states an outcome it was measured producing, not a mechanism.** The
  first row rests on the reading that could not separate the cleared flag from
  the closed verdict — and does not need to, because unifig never sends one
  without the other: `clearReturnRuleRequest` clears exactly when the verdict
  closes. What the operator is told is what the apply was watched doing. Issue
  #40's second box stays open, because it is a question about the Controller.
- **The fourth row is narrow but reachable, and that is why it is a row.** After
  #37's fix, a policy unifig created allowing and later blocked carries the flag
  false, so unifig's own policies do not reach it. What does is a policy someone
  made in the Controller's UI as an allow and later blocked there — it keeps the
  standing request, and unifig can update it. The Controller's own predefined
  policies carry the flag true while blocking too, and those are unreachable for
  a different reason: their `_id` is a composite the write endpoint answers 404
  to (issue #41).
- **On a policy the Controller ships, the note rides a change that cannot be
  applied**, and that is issue #41's rather than this one's. Blocking the
  recording's `Allow mDNS` plans an update carrying "the Controller will also
  delete `Allow mDNS (Return)`" — and the PUT underneath it answers 404, because
  a predefined policy's `_id` is a composite the write endpoint does not resolve.
  The whole Change was already unapplicable and already marked Risky; what this
  adds is one more sentence to a plan that is wrong for a reason it states
  nowhere. Nothing is done about it here for `updateFirewallPolicy`'s own stated
  reason: what a plan should say about a policy it cannot write is #41's to
  decide, and answering it in a note about return rules would be answering it by
  accident.
- **A plan can say that unifig does not know**, and this is the first place it
  says so on a single change rather than in a Caveat about a whole plan. It is
  the same admission `unreadableGateway` makes — an absence with a reason is
  invisible unless something says it out loud — moved from the end of the plan to
  the line it is about.
- **Under a policy's key, an update can only ever change the verdict.** The key
  is the name and both zones (ADR-0001), so any other edit is a create and a
  prune rather than an update. That is why the note has a field to hang off
  whenever it has something to say, and why nothing here has to consider a
  companion whose name no longer matches its parent.
- **A note needs a change to hang off, so an operator who edits nothing is told
  nothing.** The issue's complaint is that `plan` is clean in both histories.
  This makes it unclean where the verdict moves — which is every case unifig can
  put a line in front of an operator for, because under a policy's key an update
  is a verdict change and nothing else. The operator whose policy already reads
  `allow` and whose config says `allow` still gets "No changes", and their
  firewall still differs from the one that config would build fresh. Closing
  *that* means reporting on a policy nothing is changing, which is a different
  feature — drift in state unifig does not manage — and not one issue #40 asked
  for. It is the honest remainder of this decision.
- **The two paths still disagree, and that is now visible rather than fixed.**
  Issue #40 stays open on its first two boxes, which need the router. What
  changes is that an operator on the second row finds out from the plan instead
  of from the Controller.
- The standing limitation is the standing one: a single household's router on
  10.5.67, and `docs/COMPATIBILITY.md` is where that lives.
