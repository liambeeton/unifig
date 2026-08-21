# A stored policy outranks the generated one sharing its key

A Firewall Policy's key is its name together with the pair of Zones it governs
(ADR-0001), and a policy the operator stored can carry the same key as one the
Controller generates. `uniquelyKeyed` called that the ambiguity with no answer
and refused the site for it — export outright, and plan the moment the config had
a `firewall-policies:` section at all.

**The decision: the stored policy takes the key.** The generated one is
*shadowed* — still read, still spared by prune, still left out of the file, and
no longer counted as one end of a clash. What stays refused is a key more than
one *stored* policy carries.

The refusal was a refusal over a state ADR-0027's own way out invites an operator
to create. That caveat tells them to write a policy of their own on the pair,
under a name of their own; an operator who kept the name — and the UniFi UI in
front of them is where that name comes from — built exactly this, and unifig
stopped working on their whole site.

## What was measured

Issue #46, against the live migrated UDR on Network 10.5.67, 20 August 2026. The
reading is [#46's comment](https://github.com/liambeeton/unifig/issues/46#issuecomment-5358594548);
what it settles is this.

**The state is reachable.** A hand-built `POST` carrying a Generated Policy's own
name and pair — `Block All Traffic`, `Dmz -> Dmz`, `BLOCK` — was answered `201`,
and the collection went 86 -> 87 with both objects live side by side:

| | Generated | Stored |
| --- | --- | --- |
| `_id` | composite, 58 characters | `6a872517eee05071b62f10bd`, a document handle |
| `index` | `2147483647` | **`10000`** |
| `predefined` | `true` | `false` |
| name / pair / verdict | identical | identical |

**The Controller has answered which one the site is about.** Lower `index` is
evaluated first, and the shipped ruleset is only coherent under that reading: on
`Hotspot -> External` the Controller ships `Block Unauthorized Traffic` at `30003`
beneath a catch-all `Allow All Traffic` at `2147483647`, and if the higher index
won, the catch-all would swallow it and hotspot authorization would never block
anything. `2147483647` is the lowest precedence there is. The `10000` the
Controller assigned the create — unasked; unifig sends no index — is below even
the `30000` band its shipped *specific* policies sit in.

So the question `uniquelyKeyed` says has no answer has one, and it is not
unifig's guess: it is the Controller's own ordering. **Not measured by traffic,
and not claimed as such** — `Dmz` holds no networks, which is the property that
made the pair safe to probe.

## The gate is the `_id` shape, not `predefined`

Whichever policy the *site* enforces, the one unifig can be talking about is the
one it could write to, and on this clash exactly one can be. An entry matched to
the generated policy could only ever produce ADR-0027's caveat; the same entry
matched to the stored one is a change unifig makes and can make again. That is
the whole asymmetry, and it is the `_id` question — is there an object to write
to — rather than the marker's — whose object is it.

This is ADR-0027 and ADR-0028's distinction arriving in a third place rather than
a new rule, and it fails in the same direction if collapsed. A marker test would
put a policy an operator wrote and the Controller marked its own on the losing
side of a precedence it should win, and it would shadow forever the policies of a
firmware that stored its own properly — the mistake issue #34 corrected once
already, when `attr_no_edit` was read as a statement about which zones may be
edited and marked nothing of the kind (ADR-0019).

**The index is the evidence, not the test.** A match resolved on `index` would be
unifig reading a value the Controller chose unasked, measured exactly once, on
one create — and it would need a field `go-unifi` gives no reason to model. The
`_id` shape is a structural fact about every policy anyone has read, and it is
already the predicate three other decisions ask.

## Two stored policies are still refused, and two generated ones are too

Nothing has answered a key two *stored* policies carry. Both can be written to,
so unifig would be guessing which one the file meant, and that guess is what
`uniquelyKeyed` was built to refuse. The count and the naming of that refusal are
unchanged.

A key carried only by policies the Controller generates stays refused — kept
rather than chosen, since nothing has answered that one either and the issue
asked nothing of it. It is a firmware nobody has met: the 86 a migrated router
ships carry 86 distinct keys, which is why export against #46's restored baseline
is healthy. unifig can write to neither end, so the operator has nothing to
rename or remove, and this ADR says plainly what that costs rather than leaving
it to be found.

What makes the refusal honest anyway is that the message's way out reaches it:
**a policy of their own on that name and pair takes precedence over every
generated policy carrying it**, and the key resolves. Precedence turns "you
cannot fix this" into one create.

**The rejected alternative is matching one of them and moving on.** Rejected
because there is no answer to pick between them with: neither has an id, so the
`_id` gate is silent, and any tie-break — first listed, lowest index — would
decide which policy's fields the caveat compares against and therefore whether a
caveat is said at all. That is unifig guessing on the operator's behalf about
which of two objects their file meant, which is the one thing this refusal exists
to prevent.

## The message names the end that can be acted on

> rename or remove the extras in the Controller's UI

does not say which of the clashing policies is the removable one, and on the
clash it was most likely to fire on, one end **could not be touched at all**: a
Generated Policy has no id any endpoint resolves, so it can be neither renamed
nor deleted, and the UniFi UI has nothing to offer for it either (ADR-0027).
Sending an operator to the UI to remove it was sending them to do something that
cannot be done.

The sentence keeps its count, its key and its way out, and says two more things:

- **whose the clashing policies are, per key** — `2 of your own matching "X" (A
  to B)`, or `2 the Controller generates itself matching …`. It is said on the
  item rather than in the advice because it is a fact about that clash and a site
  can hold both kinds at once, and it is what makes "rename or remove the extras
  **of your own**" an instruction the operator can carry out. It doubles as why
  the count can be smaller than what the UI shows: a shadowed policy is not
  counted.
- **a policy the Controller generates has no id, so it can be neither renamed nor
  deleted, and one of your own sharing its name and pair takes precedence over it
  rather than clashing with it.** On a clash between the operator's own policies
  this is context; on a clash where none of them is theirs it is the whole
  answer, and it is a create rather than a deletion nobody can perform.

**The rejected alternative is one sentence for both clashes.** It was written
first and it could not be made true of either: worded for the operator's own
policies it tells someone with nothing to remove to go and remove something, and
worded for the generated ones it is noise on the clash that actually fires.

## The index and the refusal are one function

`uniquelyKeyed` refused, and a separate loop built the key index the plan matches
against. They asked one question from either side — which policies is the site
refused over, and which policy does a key mean — and precedence is exactly where
two such halves come apart: a shadowing rule applied in one and not the other
gives a site unifig plans against one policy and refuses to export, or the
reverse. They are `policiesByKey` now, which returns the index or the refusal.

That is the same reasoning ADR-0028 put behind `returnRule` being read by both
export and prune, applied to matching.

## The silent plan stays silent, and that is the second decision

#46 measured a worse thing than the refusal: on the maintainer's own
freshly-exported file, `unifig plan` printed `No changes. The Controller already
matches the config` while `unifig export` was exiting 1. `planFirewallPolicies` is
only reached when the config has a `firewall-policies:` section (ADR-0006), and
after ADR-0028 a migrated router's exported file has none. The operator would
have learned about the clash the first time they added any policy entry, from an
error naming a policy their entry had nothing to do with.

**Nothing changes here, and the reason is that the asymmetry is two questions
rather than one inconsistency.**

- **Plan's silence is a true statement.** The config states what unifig manages
  (ADR-0006). A file with no `firewall-policies:` section manages no policies, so
  no clash between two live policies changes anything about what that plan will
  do, and `No changes` is what will happen.
- **Export's refusal is about export's own output.** It writes the file, and a
  file naming two policies unifig cannot tell apart is a file it cannot plan
  afterwards. Refusing to hand one over is the only honest answer.
- **The state that made the asymmetry alarming is the one this ADR stops
  refusing.** #46's clash was a stored policy over a generated one, and from here
  that site exports and plans alike. What is left is two stored policies on one
  key — a state an operator built deliberately in the UI, on their own two
  policies — and the moment their file manages any policy at all, plan refuses
  exactly as export does.
- **Reaching the check would mean reading a collection the file says nothing
  about.** With no policy section and no zone prune, a plan never lists the
  policies at all. Refusing there would put a request on every run of every
  config, to police a part of the site the operator did not ask unifig to manage.
- **And it would be the failure ADR-0027 already rejected**: one clash, anywhere
  on the site, stopping every zone, network, WLAN and port-forward change in a
  file that is otherwise entirely fine.

**The rejected alternative is a caveat rather than a refusal** — plan reads the
policies anyway and says the site holds a clash. Rejected on ADR-0012's standing
objection: it fires on every run of a file that is working correctly, about a
section the operator never asked unifig to manage, and a warning that always
fires is one nobody reads. The notice that protects nobody is the notice on the
run that had nothing to say.

## Considered Options

- **Keep refusing.** Rejected: it is the status quo, and what it refuses is a
  state the Controller accepts, resolves by its own ordering, and ADR-0027's
  caveat talks operators into building.
- **Gate the precedence on `predefined`.** Rejected above: right answer, wrong
  question, and it inverts on the firmware it would matter for.
- **Gate it on `index` — lowest wins.** Rejected above: one unasked-for
  measurement, and a field unifig otherwise has no reason to model. It is the
  evidence for this decision, not its test.
- **Match the stored policy and warn about the shadowed one every run.**
  Rejected: the operator did this on purpose, on their own policy, and it works.
  A caveat on a correct configuration is ADR-0012's failure mode.
- **Export the shadowed policy so the file records it.** Rejected: it has no id
  to write to, so an entry naming it is a line no plan may act on — ADR-0028
  unchanged, and being shadowed is not being described.
- **Refuse two stored policies with a new, gentler message.** Rejected: that
  clash is unresolved, and softening the only sentence that stops unifig guessing
  would be softening the guarantee.

## Consequences

- **Prune is untouched, and had to be checked rather than assumed.** The
  shadowed policy is spared twice over — on its `predefined` marker (ADR-0005)
  and on having no id to send a deletion to (ADR-0028) — and sharing a key with a
  policy prune just deleted changes neither. The stored policy is the operator's
  own: absent from the file, it goes, exactly as it did before it had a twin.
- **Export is untouched.** The stored policy is written because a plan can act on
  it; the shadowed one is left out because no plan ever could, and it is still
  counted on stderr. Being shadowed is not being described — nothing in the file
  is a statement about it, and the entry beside it is a statement about the
  operator's own policy that happens to share its key.
- **ADR-0027's way out gains a second reading rather than losing one.** The
  rename is still the advice, and still the only path unifig can walk itself: an
  entry under the generated policy's name has that policy's key, so
  `planFirewallPolicies` matches it and caveats rather than creating (issue #43).
  What changes is what happens when an operator makes that policy in the UI
  instead — the Controller takes it, and unifig now works with the site rather
  than refusing it.
- **The e2e stand-in states the measured shape.** `seedStoredPolicy` carries what
  #46 read off the wire — `predefined: false`, a document handle, and `index:
  10000` — beside `seedGeneratedPolicy`'s composite. The index is read by nothing
  and stated anyway: it is the evidence the precedence rests on, and a fixture is
  where this repository keeps one (ADR-0019).
- **The boundary is pinned by a fixture of a firmware nobody has met.** Two
  generated policies on one key is seeded, and it stays refused. Without that
  test, "stored beats generated" and "generated policies do not count" look
  identical, and the second is a rule that would silently pick between two
  objects unifig cannot write to. That is `seedUnmarkedGeneratedPolicy`'s
  arrangement rather than a new liberty with ADR-0019 — the fixture states a
  boundary in unifig's rules, and claims nothing about what a router answers.
- **The refusal message is now built per key rather than being one constant.**
  Whoever adds a third kind of clash has to say whose the policies are, because
  the advice that follows is addressed to whoever can act on them.
