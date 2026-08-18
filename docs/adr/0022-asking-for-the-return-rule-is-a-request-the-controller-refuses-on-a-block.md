# Asking for the return rule is a request, and the Controller refuses it on a block

Issue #36 was filed off a table. Running #35's probe on hardware left one policy
behind long enough to notice that all eighty-six policies the Controller ships
carry `create_allow_respond: true` and the one unifig had created carried
`false` — because `go-unifi` v2.3.0 models the field without `omitempty`, so it
goes out on every create whether unifig names it or not, and unifig named
nothing. The Go zero value was the decision nobody made.

What the field *did* was never in the table. #36 said so plainly: "What is **not**
measured here is the field's semantics. Nobody has sent traffic through a
unifig-created allow policy and watched what comes back." Its first acceptance
box asked for that, and said it needed hardware and a human at it.

It turned out to need neither traffic nor a human watching packets. The field is
not about traffic at all.

## What was measured

Against the live migrated UDR on Network 10.5.67, 18 August 2026, from a baseline
of 86 policies — all `predefined`, none custom, every one `create_allow_respond:
true`. The probe pair throughout is `Dmz` -> `Dmz`, which holds no networks at
all, so nothing rides it.

**It is a request made at creation, not a property of the policy.** One `allow`
policy created with the field true took the site from 86 policies to **88**. The
second was none of unifig's doing:

```
name:                  unifig-probe-36 (Return)
connection_state_type: RESPOND_ONLY
predefined:            true
index:                 30000
origin_type:           custom_firewall_rule
origin_id:             <the _id of the policy unifig created>
```

The Controller generated a companion return-traffic rule, named it after its
parent, and back-referenced the parent through `origin_id`. That is the same
shape as the twelve `Allow Return Traffic` policies a migrated router ships, each
sitting on the reverse pair of one of its own forward rules — those are
companions too, and the recording had been showing this the whole time.

**The flag is the cause, not a correlate.** The same create with the field false
made **87** and no companion. One variable, same pair, same verdict: this is why
the ADR can say the field does it rather than that the two occur together. It is
the discipline ADR-0019 arrived at the hard way, applied before writing anything
down rather than after.

**The Controller reclaims the companion with its parent.** Deleting the policy
unifig created returned the site to exactly 86, ID for ID. Nothing orphaned,
which matters because the companion is marked `predefined: true` and unifig's
prune therefore spares it (ADR-0005) and would not count it as holding a zone
back (issue #28). Had the Controller kept it, unifig would have been leaving
policies behind that it could never clean up.

**The Controller refuses the request on a verdict that closes a path**:

```
400: Firewall policy create respond traffic not allowed
```

An apply with a `block` and a `reject` policy to create applied **neither**, and
said so. There is no traffic to return for a policy that blocks, and the
Controller does not treat the request as merely redundant — it rejects the body.

**The update path neither refuses nor generates.** A policy created `block` and
then updated to `allow` ends up allow, carrying `create_allow_respond: false`,
with no companion and no error. `mergeIntoStoredPolicy` sends back the stored
object with only the four fields unifig owns overwritten (ADR-0021), and this is
not one of them.

## The bug this found was unifig's own, and hours old

The measurement above was taken while implementing #36's second acceptance box,
which offered a choice: set the field to match the Controller, or stop claiming
parity in the comment. Set-to-true was chosen, committed (`992ecdb`), covered by
a request-shape test, and reviewed on both axes.

It broke every `block` and `reject` create. Setting the field unconditionally
means asking for the return rule on policies the Controller refuses to make one
for, and the first apply on hardware failed on the first such policy.

Nothing in the repository could have caught it. The e2e suite runs against the
replay stand-in, which stores whatever it is handed, so a create it would refuse
on hardware is a create that passes there — the exact gap ADR-0019 identified
when it said the cover for a payload the Controller will not take has to be a
request-shape assertion rather than a round-trip. The request-shape test written
for #36 asserted the value was `true`, which was the defect, stated confidently
and pinned.

The zone endpoint's refusal is a field it has never heard of, so
`refusedByZoneWrite` names fields. This one is a **combination** the Controller
understands perfectly well, so `refusedByPolicyCreate` is a predicate over the
body. It is the policy endpoint's first measured refusal, and it is here on the
same standard ADR-0014 set: only what was measured. `block` was sent and refused;
`reject` never reached the wire, because the apply stopped at the first failure.

## Considered Options

- **Leave `create_allow_respond` false**, as it was before #36. Rejected: it is
  the state the issue was filed about, and it is now known to mean unifig's allow
  policies carry no return rule while every one of the Controller's own does.
- **Set it true unconditionally.** This was done and is now measured to be
  broken; it cannot create a blocking policy at all.
- **Ask for the return rule on an allow and not otherwise** — chosen.
  `newFirewallPolicy` takes the policy it is building, which is what lets it know
  the verdict. It is the rule the Controller's own message states, rather than a
  list of verdicts anybody watched it refuse.

## Consequences

- **An apply creates two policies where the plan names one, and the plan must say
  so.** This is ADR-0014's own standard — a plan is a statement about what will
  happen — and it is the defect #32 fixed for zone membership, in a new place.
  The companion is predictable enough to state exactly: named `<name> (Return)`,
  generated on an allow, reclaimed with its parent.
- **An allow policy's shape depends on its history, and nothing shows it.** One
  created allow has a companion; one created blocking and later updated to allow
  does not. Two operators applying the same config file get different firewalls
  depending on what their policy used to be. Whether unifig should own the flag
  on the update path — re-deriving it when the verdict changes, so the companion
  follows the config rather than the history — is a real design question this ADR
  does not settle. Filed as **issue #40**, with the two readings that would
  settle it and the options it would choose between.
- `origin_type` has a second value. ADR-0021 recorded `network_config` on the
  policies a migrated router ships; a companion carries `custom_firewall_rule`.
  Both are back-references to whatever made the policy, and neither is
  documented by Ubiquiti.
- **The stand-in can refuse a policy create.** `refusedByPolicyCreate` reproduces
  the 400 exactly, and putting the regression back fails three tests that pass
  with the fix. The refusal list stays as narrow as the measurement.
- What none of this says is what the companion does to packets. Nobody has sent
  traffic through one, and the reading that a `RESPOND_ONLY` rule named "Return"
  carries the reply is inference from its shape and its name — better founded
  than #36's original inference, and still inference. It does not need settling
  to know what unifig should send, which is why this ADR closes the issue's first
  box on a different question than the one it asked.
- The standing limitation is the standing one: a single household's router on
  10.5.67, which is why `docs/COMPATIBILITY.md` exists.
