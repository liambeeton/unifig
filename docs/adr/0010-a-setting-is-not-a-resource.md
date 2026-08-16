# A Setting is matched by its slot, and a plan speaks of kinds rather than Resources

WAN slots are the first Setting, and the engine they arrived in was built entirely for Resources. Two things had to give, and both are about identity rather than mechanism.

**A slot is matched by `wan_networkgroup`.** A Resource is matched by a natural key the operator chose — a network's name (ADR-0001). A Setting cannot be, because the thing being matched is a piece of the router: an operator who renames `WAN` to their ISP in the Controller's UI still has the same primary uplink, and a key they can edit is precisely what a fixed slot must not have. The Controller offers three candidates. `name` is out for that reason. `attr_hidden_id` looked right, being the Controller's own marker in the spirit of ADR-0005, but it is stamped only on the primary WAN — a second slot created through the API comes back without one. That leaves `wan_networkgroup` (`WAN`, `WAN2`, `WAN_LTE_FAILOVER`), which the Controller validates, populates on every WAN entry, and uses to mean *which uplink this is*.

Which slots exist is likewise the router's answer and not the schema's. The config's `slot` is constrained to the shape the Controller's own field rule allows rather than to a list of slots unifig believes in, so a router with a third uplink can be described; naming a slot the Controller does not have is reported when unifig reads it, listing the ones it really has. The alternative — a fixed enum — would have let `export` write a file its own `validate` then rejected, which is the brownfield promise broken by a hardcoded list.

Two entries claiming one slot are the Setting's version of the duplicate-natural-key rule: unifig stops and says which slot is ambiguous, because which one the operator means is a fact only they hold.

**A plan lists kinds, not Resources.** `Change.Resource` was a lie the moment a WAN slot could be in a plan: `CONTEXT.md` defines a Setting partly by *not* being a Resource and says so in as many words. The type became `Kind`, and the JSON field with it — `{"kind": "wan"}` rather than `{"resource": "wan"}`. That is a breaking change to the machine face (issue #1, story 29), taken now because pre-v1 is when it costs nothing and because the alternative is shipping v1 with the glossary contradicted in every plan a pipeline parses. `Kind` is in the glossary alongside the terms it covers.

## Considered Options

- **Match a slot by the entry's name** — rejected: it is editable in the UI, and a Setting whose identity an operator can rename is a Setting unifig loses track of the first time they tidy up.
- **Keep `resource` in the JSON and rename only the Go type** — rejected: the output an operator reads and a pipeline parses is exactly where the vocabulary has to be right; renaming only the private half fixes the half nobody sees.
- **Model WAN slots as Resources whose create and delete happen never** — rejected: it moves the "this cannot be created" rule from the type into every planner, prune, and export path, where each one has to remember it.
- **Enumerate the valid slots in the JSON Schema** — rejected, though it would have given editors a completion list: it makes unifig's schema the authority on what hardware exists, and gets it wrong for any router with a third uplink.

## Consequences

- Encrypted DNS (issue #7) is a singleton rather than a slot, so it inherits the kind vocabulary but needs its own answer to "what identifies it" — the question is now a known one to ask of each Setting.
- The plan's ordering table (`kinds`) is keyed by kind, so a Setting sorts among Resources by the same rule. A WAN slot sorts last, which is a statement about risk rather than dependency; that is recorded in ADR-0009.
- Renaming a slot's entry in the Controller's UI is now a safe thing for an operator to do, and renaming a network is still a replacement. The two behave differently on purpose, and the reason is the difference between a Setting and a Resource.
