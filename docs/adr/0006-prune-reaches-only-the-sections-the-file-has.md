# Prune reaches only the sections the config file has

`--prune` deletes live Resources of a managed type that the config does not name, and the question this settles is what happens to a type the config does not mention *at all*. A file with a `networks:` section and no `wlans:` key states nothing about WLANs, so unifig manages none of them and prune deletes none of them. `wlans: []` is the other statement — there should be none — and prune acts on it.

This is ADR-0004's rule one level up. There, a modelled field the file omits is unmanaged rather than a request to empty it; here, a whole section the file omits is unmanaged rather than a request to empty it. Both come from the same place: the file states what unifig manages, not what may exist.

What forced the decision was adding the second resource area. Every config written while networks were the only thing unifig reconciled has no `wlans:` key, and under an unconditional prune the first `unifig apply --prune` after upgrading would have proposed deleting every WLAN on the Controller. That is not a hypothetical migration hazard — it is the shape of every future resource area too, since each one arrives after some configs already exist without it. Under this rule, adding an area can never widen the reach of a flag on a file that predates it.

The distinction is carried by a nil slice against an empty one, which Go and yaml.v3 draw exactly where it is needed and neither type-checks nor documents. `internal/config` therefore asserts it directly (`TestAnAbsentSectionLoadsAsNilAndAnEmptyOneDoesNot`) rather than leaving the whole rule resting on an unstated library behaviour, and nothing in `config.Load` may normalise a nil section into an empty slice.

## Considered Options

- **Prune every managed type unconditionally** — rejected above: the first `--prune` after any new area ships is destructive in a way the operator did not ask for and could not have anticipated from their file.
- **A per-type flag, e.g. `--prune=networks,wlans`** — rejected: it asks the operator to restate in flags what their file already says, and the two would drift. The file is the statement of what unifig manages; the flag is only whether unmanaged things are at stake.
- **Require every section to be present, even empty** — rejected: it makes the common file an error and turns adopting one area into writing boilerplate for six others.

## Consequences

- `unifig export` writes only the sections the Controller actually has content for, so a freshly exported file scopes prune to exactly what was there — and a Controller with no WLANs produces a file that puts none at stake.
- An operator who wants prune to reach a section they have nothing in writes `wlans: []`. That is the only way to say it, and it says it plainly.
- Each new resource area inherits this without doing anything: `ComputePlan` checks for the section once, in one visible place, and skips planning it entirely when the config has no key for it. The check lives there rather than inside each area's planner precisely so that adding the next area cannot accidentally widen prune's reach by forgetting it.
- Skipping the plan does not always mean skipping the read. Networks are read whatever sections the file has, because a WLAN states its binding as a network *name* and the Controller stores it as an *ID*, and nothing can translate between the two without them. What the file's sections decide is what unifig will *change*, which is the part that matters here.
