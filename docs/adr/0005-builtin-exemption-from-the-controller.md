# Built-in objects are exempt because the Controller says so

Prune deletes live Resources of a managed type that the config does not name, and some of them cannot be deleted at all: the built-in Default network is the Controller's own, and destroying it would take the site's LAN with it. Unifig decides which those are by reading the marker the Controller puts on the object — a networkconf that may not be removed carries `attr_no_delete` — rather than by holding a list of built-in names.

The two look equivalent for one network on one firmware. They stop being equivalent as soon as anything moves. A list is unifig's guess about another product's internals, and it is wrong the first time Ubiquiti adds a built-in, renames one, or ships one under a localised name; the failure mode is prune deleting something undeletable, which the Controller refuses, so an apply stops halfway through a plan the operator already approved. The marker is the Controller's own answer to the exact question being asked, it arrives with the object on the same read prune already does, and it is correct on firmware unifig has never seen.

It is also not a networks decision. Issue #8's built-in firewall Zones are the same idea wearing a different type — matchable from config, never prunable — and they carry the same kind of marker. Reading it is one predicate per resource area; a list would be one list per area, each needing to be right about a different corner of the Controller.

## Considered Options

- **A list of built-in names in unifig** — rejected above: it encodes a guess about someone else's product, and goes stale silently.
- **Attempt the delete and treat the Controller's refusal as the exemption** — rejected: the plan would advertise a deletion that cannot happen, and an apply would fail partway through rather than never proposing it. A plan has to be a statement about what will happen.
- **Refuse to prune at all while any exempt object is unlisted** — rejected: it makes the common case (a config that has simply never mentioned Default) an error, and the operator's only way out is to write config for a Resource they do not want to manage.

## Consequences

- Prune skips exempt objects silently. They are not changes, and a plan is a list of changes; an operator who wants to know what is on the Controller runs `export`.
- The exemption is per Resource, not per type. A managed type is prunable in general and an individual object may not be, which is exactly how the Controller models it.
- Config may still name an exempt object and manage its fields. Exemption is from deletion, not from being matched — `- name: Default` with a subnet is an ordinary update.
