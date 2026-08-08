# codecrawl

Working-tree **code operator** index (SCM repo tools, not session memory).

## Capabilities

- Multi-worker crawl → durable `code-index.gob`
- Stamp+hash warm refresh (zero body read when stamps match)
- Search, find-relevant, expand, impact, find-route (heuristic authority)
- Watch: fsnotify or poll

## CLI

`product-brain code-index|code-search|code-watch|…`

Exact P5 defs/refs live in sibling **codeindex** (`code-exact`).

## Non-goals

Session latent memory — [SCM-SESSION-PRODUCT.md](../../../../docs/specs/product/SCM-SESSION-PRODUCT.md).
Git-object authority for company ACL code is **ingestion** + authority process.
