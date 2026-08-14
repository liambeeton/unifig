# Stateless reconcile — no state file

Unifig diffs YAML config directly against the live Controller on every run; there is no state file. With a single Controller as the sole source of live truth, Terraform-style state adds its worst failure modes (drift, loss, secrets-at-rest) while solving a mapping problem we instead handle with per-type natural keys (e.g. match by name; hard error on live duplicates).

## Consequences

- Deletion of unlisted Resources cannot be inferred from state and is only performed under an explicit `--prune` flag.
- Controller IDs never appear in config files; the YAML stays fully human-owned.
