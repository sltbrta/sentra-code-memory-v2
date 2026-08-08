# continual

Product **continual document ingest**: watch a docs dir/jsonl and call
`ContinualDeltaLocal` on change (then gardener enqueue).

## CLI

```bash
product-brain watch --dir <brain> --docs <jsonl|directory>
```

Not a server sync. Does not upload to Neon/Qdrant.
