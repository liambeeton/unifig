# Stateless reconcile — no state file

Unifig diffs YAML config directly against the live Controller on every run; there is no state file. With a single Controller as the sole source of live truth, Terraform-style state adds its worst failure modes (drift, loss, secrets-at-rest) while solving a mapping problem we instead handle with per-type natural keys (e.g. match by name; hard error on live duplicates).

## Consequences

- Deletion of unlisted Resources cannot be inferred from state and is only performed under an explicit `--prune` flag.
- Controller IDs never appear in config files; the YAML stays fully human-owned.
- **The natural key is per type, and "match by name" is the common case rather than the rule.** Written out, because the parenthesis above has been read as though it said every type matches on a name:

  | Type | Natural key |
  | --- | --- |
  | Network, WLAN, Firewall Zone | `name` |
  | WAN slot | `slot` — the Controller's own name for a physical uplink, not one an operator chose |
  | Encrypted DNS | none; it is a singleton Setting (ADR-0010, ADR-0012) |
  | Firewall Policy | `name` **and** the ordered pair of zones it governs |

  A Firewall Policy is the one that had to be found out rather than reasoned out. The Controller ships its predefined policies one per ordered zone pair and reuses names across them — a migrated UDR answers with nineteen called "Allow All Traffic" — so a key of `name` alone finds one policy where there are nineteen, and the hard error on live duplicates fires on every migrated site (issue #24). Adding a type means asking what identifies one of them on real hardware; the answer is not reliably the name.
