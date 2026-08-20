# Export writes only the policies unifig could change

> **Amended by issue #47 on how far its guarantee reaches, not on its decision.**
> "Prune's exemption gains a second clause" below stops at the Generated Policy,
> because the Return Rule exclusion was one this ADR deferred — *"Nothing here
> settles it"*. Issue #45 then shipped **two** exclusions, and only the first had
> a clause in prune to match it. The companion had been spared by `named[key]`
> for as long as export wrote it — `Allow Return Traffic` twelve times over, and
> `"<name> (Return)"` beside any allow policy of the operator's own — and from
> #45 it was a live policy that is keyed, absent from the config, and matched by
> no exemption of its own. `sparedFromPrune` now asks `returnRule` beside the
> rest, on the same predicate export excludes a companion on, so the two halves
> read one field and cannot drift. The clause is inert on every companion anyone
> has measured, for the same reason the `_id` clause is inert, and what it is
> there for is the sentence this ADR already ends on: **a file unifig wrote must
> not be a file prune deletes from.** The reasoning stays with the code that does
> it, as "What this does not decide" asked.

`unifig export` writes every live Firewall Policy it can word into the config,
and on a migrated router that is all 86 of them. This repository's own
`unifig.yaml` is the evidence and the injury: 19 `Allow All Traffic`, 16 `Block
All Traffic`, 12 `Allow Return Traffic` and the rest, 86 entries, not one of
which unifig can change.

ADR-0027 measured why not one of them can be changed, and the measurement stays
there: a Generated Policy has no id to write to. What that ADR settled was what a
*plan* does with a change to one — it holds the change back and says so as a
Caveat. What it left open is what an *export* does with the policy in the first
place, and the answer does not follow from the first, because unifig can describe
such a policy perfectly well. It does it every time it prints one in a caveat.

**The decision: export leaves the policy out anyway.** Not because it cannot be
worded, but because an entry naming one is a line no plan may ever act on. The
config file states what unifig manages (ADR-0006), and a file naming 86 policies
it has nothing to write to is a file claiming to manage what it cannot change.
Every outcome that entry can produce is a caveat — edit its verdict and the plan
holds the change back, leave it alone and it sits there implying it could be
edited. Export is the brownfield adoption path, and the file it hands over should
be one the operator can use.

The scope of the exclusion is the id shape, which is the predicate the update
path already asks (`generated` in `internal/reconcile/policy.go`), and not the
`predefined` marker beside it. Those are different claims — whose object it is,
and whether there is an object to write to — and reading a marker to decide what
a write can do is the mistake issue #34 corrected once already (ADR-0019). The
two agree on every policy anyone has measured, and export asks the second.

## The section disappears rather than going empty

On a site whose every policy is the Controller's own — which is every migrated
router out of the box, and this repository's `unifig.yaml` — the exported file
has no `firewall-policies:` key at all. `omitempty` on the config field already
does this; what the ADR records is that it is intended rather than incidental, so
that nobody removes the tag to make the output look more complete.

**The rejected alternative is emitting `firewall-policies: []`.** Nil and empty
are not the same statement in this file and never have been: an absent section is
unmanaged and out of prune's reach, an empty one says *there should be none* and
prune acts on it (ADR-0006). So `[]` here would not be a tidier way of writing
nothing — it would be unifig asserting a claim about the section that the
operator never made, on the section where a wrong claim is most expensive. What
keeps that claim harmless today is only that every policy on such a site is
`predefined` and prune spares it anyway — which is precisely the wrong reason for
a file to be safe, since it rests a claim unifig invented on a marker answering a
different question. The absent key makes no claim at all, which is the accurate
thing to say and the safe one in the same stroke.

That this cuts against `servers` — written deliberately *without* `omitempty`, so
that a Controller with no resolvers exports as `servers: []` (ADR-0012), as a
zone's `networks:` is for the same nil-versus-empty reason — is not an
inconsistency. There, the empty list is a fact about the Controller that the
operator's file has to be able to state. Here, the Controller has 86 policies;
the emptiness is unifig's, not the site's, and writing it down would attribute it
to the site.

## The notice counts rather than names

Every export notice that has a list to give names what is on it —
`WriteOmissions` quotes the WLANs, `WriteIndescribablePolicies` the policies,
`WritePartialZones` the zones, `WriteIndescribablePortForwards` the forwards,
`WritePartialWANSlots` the slots. This one has a list and gives a count instead.

**The rejected alternative is naming them, like the others.** A migrated router
ships 86 of these under twenty-two names it reuses across them, so the list would
be a paragraph of quoted strings — 19 identical `"Allow All Traffic"` among them,
since a policy's name is not its key (ADR-0001, issue #24) and export has nothing
shorter to print. That is the shape of message that teaches an operator to skip
the notices above it, which is ADR-0012's standing objection to a warning that
fires every time, and ADR-0027's own reason for not emitting a caveat per
generated policy. The notice that nobody reads protects nobody.

What replaces the list is the reason and the way out, which the other notices do
not carry because their subjects are not actionable: a WLAN on a non-LAN network
is not something the operator can do anything about, and a generated policy is —
they can write a policy of their own on the same pair, under a name of their own
(issue #43), and the Controller's own loses to it at `index: 2147483647`.

## Prune's exemption gains a second clause

`pruneFirewallPolicies` spares a live policy four ways: on `attr_no_delete`, on
`predefined`, on the config naming it, or on unifig having no key for it. The
marker test gains a second clause beside it — the `_id` test, the same one export
excludes on.

A deletion needs an id to send exactly as an update does, so a deletion of a
Generated Policy is a promise a plan may not make. That is ADR-0014's rule
arriving in one more place rather than a new rule: the deletion would be a line
the operator approves and the Controller answers 404 to.

**The clause is inert on every policy anyone has measured, and this ADR says so
plainly.** All 86 the migrated UDR ships are `predefined: true` *and* carry a
composite id; the one custom policy ADR-0027's probe created was `predefined:
false` with a document handle. `predefined` already spares every policy the new
clause would spare. Nothing observable changes on any router unifig has seen.

**What it is there for is the disagreement, and what changed is that the file
stopped covering it.** The exported file was the backstop: it named all 86, so
`named[key]` spared them whatever the markers said, three ways over. Export's
exclusion removes that. From here, export's test and prune's test are the two
halves of one guarantee — a policy export leaves out is a policy prune must not
delete — and if they read different fields, a firmware that generated a policy
without marking it `predefined` would have unifig writing a file that omits it
and then proposing to delete it, with the deletion answering 404 on an id that
was never a handle. **A file unifig wrote must not be a file prune deletes from.**
The existing code comment argued the other way — that a second clause would be
inert and the gap belongs to whoever meets it with a measurement in hand — and it
was right while the file named all 86. It stops being right when the file
doesn't.

**The rejected alternative is replacing `predefined` with the `_id` test rather
than adding to it.** Both are true and they answer different questions; prune's
question is whose object it is, which is what ADR-0005 settled and has not moved.
Collapsing the two is the exact confusion ADR-0027 exists to prevent, and it
would also be a silent narrowing: a policy an operator wrote and the Controller
marked its own would lose its exemption.

`zonesInUse` is untouched by any of this. A generated policy still holds no zone
back, because the Controller reclaims its own along with the zone — measured on
hardware, ADR-0019 and issue #28 — and that is a third question again.

## What this does not decide

Export will stop writing a Return Rule as well — it writes twelve of them today,
as `Allow Return Traffic` in this repository's own file — and that exclusion is
**not** this one. It rides `connection_state_type` rather than the id shape,
because what disqualifies a companion is that it is not a Resource at all, and
the reasoning belongs with the code that does it (issue #45). Nothing here
settles it: this ADR's test is the id shape, and a Return Rule is left out
whatever its id turns out to be.

## Considered Options

- **Keep exporting them and let the plan's caveat handle it** — rejected: that
  is the status quo, and it makes the caveat the routine outcome of adopting a
  site rather than the exception. It also puts 86 lines in every adopted file
  that exist only to be ignored, which is how the rest of the file stops being
  read closely.
- **Export them commented out, or under a `# unifig cannot change these` banner**
  — rejected: a comment is not a statement the config language has, so validate
  cannot check it, apply cannot act on it, and re-exporting cannot update it.
  It would be documentation shipped in the one file that is supposed to be
  nothing but a statement of intent, and the notice on stderr says the same thing
  where the operator is already looking.
- **Emit `firewall-policies: []` when nothing survives** — rejected above:
  nil and empty are load-bearing for prune, and `[]` is a claim the operator
  never made.
- **Name the omitted policies in the notice, like every other notice** —
  rejected above: 86 quoted strings under twenty-two reused names is the
  message that trains an operator past the notices around it.
- **Exclude on `predefined` rather than the `_id` shape** — rejected: it is the
  right answer to a different question, and a firmware that stores its own
  policies properly would have export dropping policies unifig could write to.
- **Replace prune's `predefined` exemption with the `_id` test** — rejected
  above: both claims are true, and prune's is the first one.
- **Warn on an existing file that names generated policies** — rejected: such a
  file works, plan is silent when it agrees with the Controller, and a warning
  firing 86 times on a correct file is ADR-0012's failure mode again.
  Re-exporting is the fix, and it is one command.

## Consequences

- **Two tickets carry this out and the order is not free.** Prune's clause lands
  first (issue #44), then export's exclusion (issue #45), so there is never a
  window in which a freshly exported file plus `--prune` proposes a deletion that
  answers 404. Issue #43's rename fix lands before the notice that repeats its
  wording.
- **This repository's `unifig.yaml` loses its `firewall-policies:` section
  entirely** — all 86 entries and the key. It is the file this ADR is about, and
  leaving it as it is would be shipping the counter-example. ADR-0027's sentence
  about "nineteen `Allow All Traffic` policies in this repository" then points at
  git history rather than at the live file.
- **The README's invariant changes verb.** "What an adoption couldn't describe is
  not something prune may delete" becomes *didn't write down*: unifig describes a
  generated policy perfectly well every time it prints one in a caveat, and the
  thing the two halves now share is what export omitted, not what it failed to
  word. That widening is what makes prune's second clause part of the same
  sentence as export's exclusion rather than a coincidence.
- **An operator's existing file that names all 86 keeps working**, unchanged and
  unwarned. Nothing here is a validation rule: a config may still name a
  Generated Policy, and what happens then is ADR-0027's caveat, exactly as
  before. This ADR is about what unifig *writes*, not about what it will read.
- **Export's stderr notices are now of two kinds**, and the split is worth
  naming for whoever adds the next one: one kind says unifig could not describe
  the object, the other says unifig could and chose not to. The second is the
  only kind whose subject the operator can act on, which is why it is the only
  one that ends in a way out.
