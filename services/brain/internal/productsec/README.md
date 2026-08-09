# productsec

**Phase 2** residual product security profiles and lightweight durability
helpers.

## Role

| Type | Meaning |
| --- | --- |
| `single_user` | Default offline brain; authorize always allows |
| `multi_principal` | Owner + grants map; deny non-disclosing (`ErrDenied`) before retrieve |

Also:

- `security.json` per brain dir (owner, profile, grants, evidence digest)
- Sealed session frames under `sessions/*.sealed` (AES-GCM, local key
  derivation)
- Evidence digest of `chunks.jsonl` for gardener non-mutation checks

## Integration

- `hosted.Client.SetSecurity` / loaded on `OpenLocal`
- `AnswerOpts` calls `Authorize("ask")` first
- CLI: `product-brain ask --profile multi_principal --principal bob`

## Partial

Chunk *bytes* still live as FS projection; full ArtifactVault migration is
[NG-VAULT-CHUNKS](../../../../docs/roadmap/DEFERRED-AND-NON-GOALS.md).
