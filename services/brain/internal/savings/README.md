# Local token-savings ledger

`internal/savings` records deterministic per-step byte, token, deduplication,
compression, avoided-read, and model-call counters without network access or a
model tokenizer. Callers supply their own token counts and project cache.

```go
ledger, err := savings.Open(projectCache)
err = ledger.Record(savings.Step{
    Name: "find-relevant", Category: savings.CategoryRetrieval,
    BaselineBytes: 1200, ServedBytes: 240,
    BaselineTokens: 300, ServedTokens: 60,
})
summary := ledger.Summary()
fmt.Println(summary.String()) // stable CLI form
raw, err := json.Marshal(summary) // stable JSON field/category order
```

The committed file is `<projectCache>/token-savings.json`. Writes use a
same-directory temporary file, sync, and atomic rename. A malformed committed
file is returned as an error and is never replaced implicitly. Use one `Ledger`
instance per cache file; it serializes concurrent records within the process.

`scmbench.Report.RecordSavings(cache)` is an optional integration after
`MeasureBaseline`. It records the report as one retrieval step because the
benchmark baseline applies to the whole scenario. Normal report and codeserve
wire output are unchanged.
