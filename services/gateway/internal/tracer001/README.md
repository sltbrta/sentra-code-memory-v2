# Tracer 001 gateway handlers

Status: **[partial] handlers with boundary tests; optional mount hooks in
localauthority; uncomposed into the live local-authority process (I0).** This
package implements the Stage 06 Tracer 001 public path as a composition facade
behind the authenticated owner-only gateway boundary. Stages 03–05 remain the
product RPC surface; this package does not invent merge/deploy messages.

## API

```go
handler, err := tracer001.NewHandler(tracer001.Config{
    Path: path, Clock: clock, ConfigurationDigest: digest,
})
resp, err := handler.Session(ctx, peer, request)
resp, err := handler.Ingest(ctx, peer, request)
resp, err := handler.Ask(ctx, peer, request)
resp, err := handler.Intent(ctx, peer, request)
resp, err := handler.Plan(ctx, peer, request)
resp, err := handler.Review(ctx, peer, request)
resp, err := handler.DraftPR(ctx, peer, request)
resp, err := handler.Outcome(ctx, peer, request)
// or: handler.Advance(ctx, peer, step, request)
```

## Boundary rules

- **Validate, then identity, before any port.** Per-step field shape and nested
  `buf.validate` rules run before the peer cross-check; mismatches return
  `ErrRequestDenied` / `ErrInvalidRequest` and never invoke the port.
- **Static non-disclosure.** Unknown, unauthorized, stale, revoked, and
  idempotency conflicts return `not_found_or_denied` with a rejected receipt
  and zero run/step/draft/outcome payloads (inaccessible ≡ absent).
- **Every success revalidated.** Constructed `TracerRun`, `TracerStepReceipt`,
  `DraftPrReceipt`, and `OutcomeFact` pass protovalidate before return.
- **No ambient credentials.** The path port owns composition into ingest/query/
  factory/broker; this package never imports brain/broker implementations.

## Ports

- `Path.Advance` — composition for all eight steps (fakes in tests; I0 wires
  real Stage 03/04/05 + draft-PR/outcome surfaces).
- `Clock` — receipt observation time.

## Verification

```sh
go test -race ./gateway/internal/tracer001           # from services/
bazel test //services/gateway/internal/tracer001:tracer001_test
```
