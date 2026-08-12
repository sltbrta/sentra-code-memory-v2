<!-- markdownlint-disable MD013 -->

# Ouroboros Go services

Go workspace for **brain**, **broker**, and **gateway**. Bazel is verification
authority; `go.work` is the developer boundary.

## Layout

| Path | Role |
| --- | --- |
| [brain/](brain/) | Product brain kernel + `product-brain` CLI |
| [gateway/](gateway/) | `authorityprocess` + `*api` RPC leaves |
| [broker/](broker/) | Authz, identity, GitHub effects, capability |
| [internal/contracts/](internal/contracts/) | Cross-leaf ports (no `internal/` leakage) |

## Product entry

```bash
# From services module
go build -o product-brain ./brain/cmd/product-brain/
./product-brain help
```

Doctrine: [ADR 0022](../docs/decisions/0022-product-only-retire-stage.md) sole
binary; [ADR
0023](../docs/decisions/0023-unified-product-durability-and-program-ladder.md)
unification ladder.

## Docs

- [ARCHITECTURE.md](../docs/architecture.md)
- [Deferred & non-goals](../docs/roadmap/DEFERRED-AND-NON-GOALS.md)
- [brain README](brain/README.md)
