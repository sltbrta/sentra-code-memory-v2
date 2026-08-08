# ontology

Typed entity/relation graph helpers (co-occurrence, document edges). Used by
gardener generation enrich and projections. Rebuildable — not primary evidence.

## Predicate packs (P1)

`packs/default.yaml` lists **multi-valued predicates** (`tags`, `aliases`, …)
that must not form claim conflict attacks. Load via `LoadPredicatePolicy(path)`;
missing file → embedded pack → hard-coded default. `AdmitClaim` / `ResolveGroup`
consult `IsMultiValuedPredicate`.
