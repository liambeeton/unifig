# Blocking the internet is not a Risky change, and blocking the Gateway zone is

> **Amended by ADR-0027 on one premise, not on its decision.** This ADR argues in
> two places that the lockout is a one-line *edit* to `Allow All Traffic` from
> Internal to Gateway, "because a predefined policy is matchable and updatable
> like any other". It is not updatable: the Controller generates its own policies
> for a pair of zones rather than storing them, and the composite `_id` that
> results is one it answers 404 to (issue #41). unifig holds such a change back at
> plan time now, so the mark below never reaches one.
>
> **The rule and its scope are unchanged**, because this ADR's other argument
> carries them on its own: the Controller's policy on a pair sits at `index:
> 2147483647`, so a policy the operator *creates* over it takes effect. The
> lockout is still one line of config; it is a create rather than an edit. What
> also survives is the warning — the caveat about the unwritable policy suggests
> exactly that create, so it carries this ADR's own risk sentence with it.

`Change.Risk` was set in one place in the codebase — the WAN planner — so no Zone
or Firewall Policy change ever carried one, and a firewall edit was planned,
approved and applied as an ordinary change (issue #26). Issue #1's story 24 asks
that "any WAN/Internet-affecting change" be individually confirmed, and two
firewall changes look like they qualify: a policy governing `Internal -> External`
flipped to `block`, and a prune of the policy that allows that pair. Both take the
site's internet away. Neither is Risky, and the one that is was not on the list.

**The management path runs to the Gateway zone, and nothing on the `External`
axis touches it.** The recording from migrated hardware holds six built-in zones,
and the Controller answers in `Gateway`: `Internal -> Gateway "Allow All Traffic"`
is one of the eighty-three predefined policies. Block `Internal -> External` and
the site loses the internet while the operator sits in front of a working
Controller UI, one field away from undoing it. Block `Internal -> Gateway` and
there is no UI left to sit in front of. That is `CONTEXT.md`'s physical-access
test, and it separates the two changes that issue #26 grouped together.

**`CONTEXT.md`'s definition had two clauses, and this is where they stopped
agreeing.** It read "can sever internet **or** management connectivity", then
"The test is whether recovery could need physical access". For a WAN slot both
clauses say Risky, so nothing forced a choice. A policy blocking `-> External`
splits them: it severs internet with the Controller still reachable. ADR-0012
already chose the second clause once, for Encrypted DNS, but on a change that
never actually severed the uplink — so it could decide the case without saying
the first clause was subordinate. It is subordinate. The entry now leads with the
physical-access test and describes losing the internet as the usual consequence
rather than a second test, because two clauses that can disagree are two rules,
and only one of them was ever meant.

**So the rule is one clause: a Firewall Policy that would newly block traffic to
the Gateway zone.** Three parts, each ruling out a way of getting it wrong.

- *To the Gateway zone*, because every other destination leaves the Controller
  reachable. This is the deliberate "no" issue #26 asked for either way: a policy
  blocking the internet is an ordinary change, and an operator writing "stop the
  IoT VLAN reaching the internet" gets no prompt.
- *Newly* blocking, because a policy already blocking that pair cannot close a
  path that is closed. `block -> reject` changes what a dropped packet is told
  and nothing about reachability, and a confirmation in front of it is one an
  operator learns to click through — ADR-0012's own warning about what makes a
  prompt stop being read.
- *Block or reject*, on a create as well as an update. The Controller's own allow
  on that pair carries `index: 2147483647`, the lowest precedence there is, so a
  new blocking policy written over it is one that takes effect.

**A deletion does not qualify, which is narrower than issue #26 assumed.** The
issue lists `--prune` deleting the policy that allows the pair. Prune cannot: all
eighty-three of the Controller's own policies are `predefined` and every one is
spared (ADR-0005). What prune can delete is a policy an operator made, which sits
*above* the predefined allow rather than replacing it — so deleting it returns the
pair to its default rather than closing it. Marking deletions Risky would put a
prompt in front of the one operation on that pair that is generally safe.

**Zone membership is out, and that is a gap rather than a proof.** Moving an
operator's own LAN out of the Internal zone can strand it somewhere with no policy
to the Gateway at all, and no policy line changes. It is left out because the rule
has to be one an operator can predict from the line they wrote: "you edited a
policy whose destination is the Gateway zone" is predictable, and "you moved a LAN
between zones and unifig decided that mattered" is not — unifig has no way to know
which network the operator is on. This is written down so that the next person to
find the gap finds a decision rather than an oversight.

## Which zone is the gateway is read from the Controller, not from a list

The rule needs to identify one specific built-in, and `default_zone` cannot say
which — it says only that a zone is the Controller's own. There were two signals
and they are not equally good.

`zone_key: "gateway"` is the Controller's own stable key for it. The alternative
was matching the name `"Gateway"`, which costs nothing to implement, needs no
request at all — the operator's own file already says `destination: Gateway` —
and is a hard-coded list of Ubiquiti's zone names. That construct is what had
`--prune` proposing to delete every built-in including `External` eleven days
before this was written (issue #23), and its failure mode here is worse: the name
list fails *silently*, marking nothing, and an operator reads a plan with no `!`
on it as a plan that risks nothing.

**This narrows a comment on `zoneMarker` that said the opposite**, and the
narrowing is the point rather than an aside. That comment refused `zone_key` on
the grounds that reading it "would be keeping a list of Ubiquiti's zones by
another route (ADR-0005)". That holds for ownership, where the Controller answers
directly with `default_zone` and a name list would be second-guessing an answer
already given. It does not hold here, because there is no answer already given:
the choice is not between the Controller's word and a list, it is between
`zone_key` and inventing the string `"Gateway"` ourselves. ADR-0005's principle is
*read the marker off the object*, and `zone_key` is a marker on the object.

Both facts come out of one response. `builtInZones` became `readZoneFacts`,
returning which zones are built-in and which of those is the gateway, because it
is one `GET .../firewall/zone` and asking twice would produce two answers about a
Controller that may have changed between them. The read was prune-only, on the
reasoning that a plan which cannot delete anything should not pay a request for
the answer; it now runs whenever a policy is read, which is the same condition
the policies themselves are read under. A plan that manages zones and cannot
delete them still pays nothing.

**A Controller that answers and names no gateway gets a Caveat, not silence.**
This is the `zoneOwnership.known` distinction arriving at a second question, and
it is here for the reason issue #23 put it there: an absence with a reason is
invisible unless something says it out loud, and a plan with no `!` on it reads as
a plan that risks nothing. The caveat is said only when the plan holds a change
the check would have looked at — something turning a verdict to `block` or
`reject`. A firewall plan that blocks nothing had no question to answer, and a
caveat on every run is one an operator reads past by the third.

## Being Risky is what puts a change last

The WAN slot is applied last because it can cut the site off, and that used to be
a property of the `kinds` table — `WANSlot` is last in it. That worked while the
WAN was the only Risky kind and stopped working here: a Firewall Policy sits at
order 3 because it must follow the zones it names, so a policy blocking the
management path would have been applied *before* the DHCP reservations, the port
forwards, the Encrypted DNS and the uplink. An operator who approved it would
watch the rest of their plan fail down a connection that no longer existed.

`sortChanges` now sorts on risk between action and kind, so a Risky change goes
last within its group whatever kind it is. It is dependency-safe because nothing
references a Firewall Policy and because a create that must precede another change
is in the earlier action group regardless; it is deterministic, so the
byte-identical-plans guarantee holds; and it changes nothing about any plan unifig
produced before this, because the only Risky kind was already last in the table.
Mostly it puts "left until the safe work is done" where ADR-0009 put everything
else about this feature: on the sentence, not on the kind.

## What unifig cannot know, and says "can" about

The risk sentence hedges — "blocking traffic to it **can** cut the path this site
is managed over" — for the same reason `wanRisk` does, and here the hedge is
load-bearing. unifig models a policy's name, verdict and pair of zones and nothing
else. It does not model `index`, so it cannot know whether a blocking policy sits
above or below an allow. It does not model `enabled`, so a policy switched off in
the UI is one it will still mark. The mark is a warning that a rule closing the
management path is being written, not a verdict on what the rule set will do.

That over-warns, deliberately. The cost of a false positive is one extra `y`; the
cost of a false negative is a UDR that needs physical access. Modelling precedence
would make the mark exact and would be a large expansion of what a Firewall Policy
means in this project — filed here as the reason `index` is visible in the
recording and read by nothing.

**The spec had already settled this, and the two hardest calls in this ADR turn
out to be scope compliance rather than judgement.** Issue #1's Out of Scope says:

> Full lockout analysis (graph reasoning about whether a change severs the
> management path) — only the Risky-change warn-and-confirm class ships.

Reading `index` to decide what a rule set actually does is that graph reasoning,
and so is asking whether a network moved between zones still has a path to the
gateway. Both were argued above from first principles, and both were out of scope
before the argument started. What ships is the warn-and-confirm class, which is
what this ADR marks: a sentence saying a rule closing the management path is being
written, not a verdict on what the firewall will do with it.

## Considered Options

- **Mark any verdict change on a pair involving `External`, plus any prune of
  one** — rejected, and it was issue #26's own leading candidate. It marks the
  ordinary "no internet for this VLAN" edit, which is a thing operators write on
  purpose, and it misses the one change that can actually lock them out. It is
  drawn on the axis that is easy to see rather than the one the test is about.
- **Mark any policy change at all, plus any zone membership change touching
  `External`** — rejected on ADR-0012's reasoning, which issue #26 correctly
  anticipated: it makes almost every firewall edit interactive, and a prompt that
  fires on everything is a prompt that gets answered without reading.
- **Decide that no firewall change is Risky, and record that** — rejected, but it
  was a real option and is what ADR-0012 did for Encrypted DNS. What defeats it is
  that `Allow All Traffic` from Internal to Gateway is a predefined policy, and a
  predefined policy is matchable and updatable like any other — only prune exempts
  it. The lockout is not a hypothetical requiring an unusual config; it is a
  one-line edit to a policy the Controller ships.
  _(ADR-0027: it is **not** updatable, and the rejection stands on the create
  instead — a policy written over the Controller's own takes effect, so the
  lockout is still one line of config.)_
- **Match the gateway by the name `"Gateway"`** — rejected, as above. Zero
  requests and no ADR-0005 collision, at the price of a construct that has already
  cost this project two bugs and whose failure mode here is silence.
- **Model `index` so the mark says what will happen rather than what is being
  written** — rejected as out of proportion. It would make unifig responsible for
  evaluating a rule set it does not otherwise model, to convert a warning that is
  occasionally unnecessary into a warning that is exact.
- **Move `FirewallPolicy` down the `kinds` table instead of sorting on risk** —
  rejected: it would reorder every firewall plan, Risky or not, and it would put
  the ordering rule somewhere that says nothing about why. Nothing references a
  policy, so the move would have been safe; it would just have been the wrong
  place to say it.

## Consequences

- The second Risky area exists, which turns ADR-0009's machinery from a
  WAN-shaped feature into a general one. `--allow-risky` now says yes in advance
  to two different things, and a pipeline that applies firewall config
  unattended needs it where it did not before. The apply reports the policy it
  left behind, exit code 0, and the next `plan` exits 2 — unchanged.
- `readZoneFacts` runs on every plan that manages Firewall Policies, where it
  previously ran only under `--prune`. That is one extra `GET` on a run that is
  already reading two collections from the same tree.
- The firewall's Risky behaviour is covered against the recording, because no
  container has a zone-based firewall to test it on (ADR-0013). The e2e suite
  mirrors the WAN's: plan prose, `plan --json`, the apply prompt answered yes and
  no, `--allow-risky`, EOF, and the apply ordering. One test renames the gateway
  zone in the stand-in and asserts the mark still fires, which is the only way to
  tell a key-based lookup from a name-based one — and it is the test a future
  refactor to "just check the name" would fail.
- **The rule depends on `zone_key` surviving `make record-udr`, and now something
  asserts that.** It does survive — the field is in no name table and matches no
  shape, so the scrub passes it through as written — but that was two functions
  happening to agree rather than anything checking. The existing test asked
  whether a kept zone had a key at all, which a scrub replacing every key with a
  placeholder would still pass. A re-recording that lost the values would disable
  every Risky mark on the firewall and no test would have said so, which is the
  silent failure this whole ADR is arranged against. The new test compares the
  keys against what the fixture sent.
- A policy unifig *creates* on the Gateway pair still has the open question
  ADR-0013 left: the recording holds only the Controller's own policies, so what
  comes back from a create is unverified against hardware. This ADR adds a reason
  to care — a created blocking policy is now one unifig marks Risky, so if a
  create does not land as sent, unifig will have warned about a change that did
  not happen.
- Zone membership can still strand a network with no path to the Gateway, and
  nothing warns. Named above as a gap; the place to fix it, if it is ever worth
  fixing, is a check that a network the config moves still has some policy to the
  gateway — which needs unifig to evaluate a rule set, the same expansion this
  ADR declined for `index`, and which #1 puts out of scope.
- **Issue #1's Risky-change bullet was amended to match, and story 24 was not.**
  The bullet said "any WAN/Internet-affecting mutation", which read literally makes
  a policy blocking `Internal -> External` Risky — the spec's copy of the
  two-clause ambiguity this ADR resolved in `CONTEXT.md`, and the sentence #26
  quoted as its justification. It now states the physical-access test and names
  both qualifying classes. Story 24 keeps its original wording with a pointer
  here, because a user story is a record of what was asked for: rewriting it would
  have the spec claim the operator always meant the Gateway zone, and working that
  out took migrated hardware and a recording.
