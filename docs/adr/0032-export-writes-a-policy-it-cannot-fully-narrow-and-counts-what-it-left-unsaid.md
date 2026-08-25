# Export writes a policy it cannot fully narrow, and counts what it left unsaid

ADR-0031 gave a Firewall Policy a **Narrowing** — a protocol from six and a list
of destination ports — and left the other half open in its own consequences: a
policy the Controller holds narrowed by something unifig has no words for exports
without it, and says nothing. That is the promise every other export notice
keeps, broken in the one place nothing was watching.

A stored policy someone narrowed to `tcp/53` in the Controller's UI exported as:

```yaml
- name: Allow DNS out
  action: allow
  source: Ellingson
  destination: Gateway
```

Apply that file to a fresh Controller and you get a policy allowing **everything**
on that pair, where the site it came from allowed one port. #51 closes that
example. What it cannot close is the shape below it: a narrowing the config has
no field for at all.

The plan side of this is not a defect and is deliberately untouched. An entry
stating no narrowing has asked for nothing about ports (ADR-0004), so reporting
no drift is correct. **Export is different, because export is the brownfield
adoption path** — a file that came back short is supposed to say why.

## What the router says, which is that this is a guard

Nothing here needed a probe, and the recording is why. All eighty-six policies a
migrated UDR ships are Generated Policies, which export never writes, and the
narrowings unifig has no words for are on those and only those:

| shape | in the recording |
| --- | --- |
| `match_opposite_ports: true` | 0 of 86 |
| `port_matching_type: OBJECT` | 0 of 86 |
| destination `matching_target` other than `ANY` | 7, all `IP`, all generated |
| source port matching other than `ANY` | 5, all `SPECIFIC`, all generated |
| protocol outside the six | 0 of 86 |

So this ships as a guard rather than as a repair, and the guard is worth having
for a reason that is new since ADR-0031: **`all` beside a `SPECIFIC` port is a
shape the Controller will hand back.** That combination was measured being
accepted and stored on the live migrated UDR, and it is the one thing unifig
refuses that the Controller takes — so export cannot write it, `validate` would
reject a file that did, and the entry left behind says the policy has no ports.

## The decision: the policy is written, the omission is counted

**Not the `WriteIndescribablePortForwards` treatment**, which drops the whole
object. The difference is deliberate and it is what this ADR exists to say, or it
reads as an oversight. A forward whose ports unifig cannot describe has nothing
left worth writing — a port is half of what a forward *is*. A policy still has a
name, a verdict and a pair of zones, and `CONTEXT.md` says export's scope is what
a Plan could act on, which those three are. The entry is true as it stands, and a
freshly exported file holding one plans clean.

**A count, not a list of names**, on `WriteGeneratedPolicies`' reasoning rather
than on its own. A paragraph of quoted policy names is what teaches an operator
to read past the notices above it (ADR-0012), and a policy's name is not its key
(ADR-0001), so the quoted list would not even identify which policy to go and
look at. The message names the *shapes* instead, because those are what an
operator can recognise in the Controller's UI:

```
Wrote 1 firewall policy narrowed by something the config cannot state.
Each matches on more than the protocol and destination ports unifig models — a
port group, an inverted match, a source port, an application or region target,
or a protocol outside the six. unifig manages the verdict, the pair of zones and
whatever narrowing the entry does state, and leaves the rest of the matching
exactly as it is.
```

It ends in a consequence rather than in a way out, which is where every notice
but `WriteGeneratedPolicies` ends. There is nothing an operator can do about a
port group unifig does not model, and the honest sentence is that unifig will
leave it alone.

**It is the notices' third kind**, which is worth naming because the first two
already had names. `WriteOmissions` and the two beside it say unifig could not
describe the object and left it out. `WriteGeneratedPolicies` says unifig could
and chose not to. This one says unifig described the object and not the whole of
it — which is `WritePartialZones`' and `WritePartialWANSlots`' shape, arriving at
the firewall.

## What counts as narrowed beyond the config

`narrowedBeyondTheConfig` reads the live policy rather than the entry, because
the shapes are invisible in the entry: a policy narrowed to a port group and a
policy narrowed to nothing at all write the same three lines.

It asks the **mode** fields at each end rather than the values under them —
`protocol`, the two `port_matching_type`s, the two `matching_target`s. The
address lists, MAC lists, app ids and regions hang off those modes, so nothing
needs a list of the Controller's matching engines, and that is the point: **a
list like that fails by going quiet.** A firmware that adds a seventh engine
would export a policy narrowed by it and say nothing, which is the same argument
ADR-0005 makes about built-in zones and ADR-0018 makes about management ports,
arriving a third time.

Each mode is read **together with what it points at**, because a mode with
nothing under it narrows nothing — and the inversions are read beside the thing
they invert for that reason and one of their own. `match_opposite_protocol` and
the two `match_opposite_ports` are plain bools with no `omitempty`, so they are
on the wire of every policy the Controller sends; a count reading one on its own
would speak for an omission that is not there, on a file that is working, which
is the notice ADR-0012 says teaches an operator to skip the rest. The address and
network inversions are not read at all, and need not be: they mean something only
beside a `matching_target` of `IP` or `NETWORK`, and every target but `ANY`
already counts. The one inversion counted rather than excused is an inverted
`all`, because a policy matching everything-but-all matches nothing and the entry
would claim the opposite.

That deliberately reaches one shape further than the issue enumerated. It counts
a destination `matching_target` of `IP` or `NETWORK` as well as the four the
UniFi UI offers under "advanced" — the same defect wearing the Controller's
oldest matching engine, and the seven in the recording are on generated policies
only by luck of this site.

The port matching is asked of `statedPorts`, the same function the entry is
written from, rather than of the fields a second time. Two halves of one
guarantee reading different fields are two halves that can drift, which is
ADR-0028's reasoning about the Return Rule applied to a smaller thing.

**One shape is dropped from the entry as well as counted**, and it is the
inversion. `ports: [53]` written off a policy carrying `match_opposite_ports`
would be the file saying the *opposite* of what the Controller holds — the one
case where writing the narrowing is less honest than omitting it. Left out, the
entry states no narrowing, which manages none, which is true.

## Considered Options

- **Drop the policy from the file, like an indescribable port forward** —
  rejected above. It would put a policy an operator manages today out of reach of
  their config over a field they may not use, and `--prune` would stop touching
  it too.
- **Name the policies rather than counting them** — rejected. It is
  `WriteGeneratedPolicies`' argument, and a name is not a key here, so the list
  would be both long and insufficient.
- **List the four matching targets the UI offers** — rejected. The failure mode
  is silence, which is the one this whole notice exists against.
- **Write the narrowing anyway, as far as it goes** — rejected for the inversion
  specifically, which is what makes it wrong in general: a partial narrowing that
  reads as a whole one is worse than no narrowing at all, because unlike an
  omission it plans clean *and* means something else.
- **Make the plan read an inverted match the way export now does** — deferred,
  not decided. See the consequences.

## Consequences

- **The plan is unchanged, and one hole in it is now written down.** A file that
  states `ports: [53]` against a policy the Controller has inverted plans as no
  change today, because `statedPorts` reads the port list without the inversion.
  Dropping it for both readers would turn that into a perpetual update — an
  update writes the ports back and leaves `match_opposite_ports` where it is — so
  neither reading is obviously right, and issue #52 scoped the plan side out by
  name. `quietInvertedMatch` is export's alone for that reason, as
  `quietWideNarrowing` already was.
- **A count that fires is not a failure**, exactly like every notice around it.
  The exit code is unchanged and the YAML on stdout is unchanged.
- **The count is of policies, not of omissions.** A policy narrowed three ways
  unifig cannot state is one line short, and it is one entry that came back
  short.
- **A Generated Policy narrowed beyond the config is not in it.** It was never
  written, so it is counted where it was left out — `WriteGeneratedPolicies` —
  and once. The two notices meet on no policy.
- **`portGroup` and `anyMatch` join the Controller's vocabulary in `policy.go`.**
  `anyMatch` is the same string as `anyPorts` and deliberately not the same
  constant: one is a port-matching mode unifig writes, the other is a
  `matching_target` unifig only ever reads.
- **Nothing here needed the router**, which is why it could ship the day after
  ADR-0031's session. The e2e cases seed each shape onto the recorded stand-in,
  including two the recording does not hold at all.
