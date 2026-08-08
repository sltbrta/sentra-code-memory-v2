# federation

**Phase 5** federated ask MVP over **local** brains.

## Flow

```text
FilterCards (authorize-before-fanout)
  → RankCards (topics + cost)
  → MintCapability (TTL)
  → OpenLocal + answer per selected card
  → merge answers with brain labels
```

## CLI

```bash
product-brain federated-ask \
  --q "…" --principal alice \
  --cards /path/a:brain-a:alice,/path/b:brain-b:bob
```

## Non-goals

Multi-host HTTPS mesh, global brain mesh, unrestricted remote prose merge —
[NG-FED-MESH](../../../../docs/roadmap/DEFERRED-AND-NON-GOALS.md).
