# chunking

Versioned chunking policy for the product brain (issue #332). Deterministic
leaf package: no storage, no network. The canonical policy contract lives in
[docs/specs/brain/CHUNKING-POLICY.md](../../../../docs/specs/brain/CHUNKING-POLICY.md).

## Strategies

| Strategy | Purpose | Baseline sizing |
| --- | --- | --- |
| `whole_doc` | Naive RAG baseline (legacy `DocumentsToChunks` shape) | one chunk per doc |
| `fixed` | Token-window baseline | 500 target / 50 overlap |
| `structure` | Boundary-aware for prose/code/table/slides/chat | 500 target / block-granularity overlap |
| `parent_child` | Precise children under expandable parents | 125/25 children, 1000/100 parents |

## Receipt contract

Every `Receipt` stamps `policy_id`, `policy_version`, `tokenizer_id`
(`ouroboros-ws-1`), strategy, kind, byte offsets into
`SourceDocument.Source()`, token count, sha256 of the chunk text, and (for
parent-child children) the parent chunk id. `Chunk` is deterministic: the
same documents + policy always produce identical receipts, so rebuild identity
is the tuple `(policy_id, policy_version, tokenizer_id, document_id, seq)`.

Mapping receipts to `hosted.ChunkWrite` is the caller's job; the existing
`ChunkStore`/`BurstUpsert` ingestion path stays the single write contract.
