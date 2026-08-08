# tenant

**Phase 4** multi-tenant **local-file MVP**: registry, brain paths, fail-closed isolation.

## Surfaces

```bash
product-brain tenant create --root <reg> --id t1 --region us
product-brain tenant brain-create --root <reg> --id t1 --brain-id b1
product-brain ask --tenant t1 --tenant-root <reg> --brain-id b1 --q "…"
```

## Isolation

`AuthorizeBrainPath(tenant, path)` ensures path is under that tenant’s brain
root. Cross-tenant `--dir` on ask emits `failure: cross_tenant_denied`.

## Non-goals

OpenFGA cloud admin, SCIM/GDPR full suite, multi-region HA — see
[DEFERRED-AND-NON-GOALS](../../../../docs/roadmap/DEFERRED-AND-NON-GOALS.md).
