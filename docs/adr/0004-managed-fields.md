# Config models a few fields; the Controller has a hundred

A Resource in unifig.yaml describes a handful of fields — a network is a name, a VLAN and a subnet — while the Controller object behind it has around a hundred, and the Internal API stores whatever it is sent rather than merging. So every write has to answer what happens to the fields the config never mentions, and the answer differs by verb: an **update** reads the live Resource, overwrites only the modelled fields and puts the whole object back, while a **create** starts from the Controller's own defaults for a new object of that type, because there is no live object to preserve and a bare struct would serialise as every feature switched off — a network with NAT and DHCP disabled is not a network.

"Mentions" is per field, not per Resource: a modelled field the file omits is unmanaged too, not a request to empty it. The schema gives no way to ask for a network with no VLAN or no subnet (`vlan` starts at 1, `subnet` must match a CIDR), so reading omission as removal would delete a setting the operator had no way to ask to delete. Removing a field's value is therefore a Controller-side operation, and `- name: X` alone is a valid entry meaning "match this Resource, manage nothing about it yet".

## Considered Options

- **Model every field** — rejected: the config file stops being human-owned, and the tool inherits the Internal API's undocumented surface as its public one.
- **Send only the modelled fields and let the Controller fill in the rest** — rejected: it does not. A create with three fields is stored with three fields, and the result is a network that cannot route or lease.
- **Write nothing unmodelled, ever** — rejected: it makes create unusable, and it does not even hold for update. A DHCP pool cannot stay in a subnet the network no longer has; the Controller rejects the whole update as an invalid range.

## Consequences

- Anything an operator sets in the Controller's UI survives an apply, so unifig can be adopted for one field of one Resource without taking ownership of the rest. This is what `--prune`'s absence means at field level.
- Create defaults are unifig's own policy and are set once. They are not re-asserted on update, so an operator's later edits win permanently.
- Where a modelled field's change strands an unmodelled one, unifig repairs the dependent field and says so in the plan. A plan that quietly did more than it printed would not be a plan, so any future case of this gets a note in the plan too, never a silent write.
- The same three rules are what each later resource area implements; the network engine (issue #3) is the worked example.
