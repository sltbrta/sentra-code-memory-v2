# Stage 13 fenced local→Modal handover

One non-effectful leaf handover with checkpoint, fence CAS, exactly-once
completion, recovery, revoke, and cleanup receipts.

```sh
just stage-exit 13
OUROBOROS_MODAL_SMOKE_MODE=hermetic just modal-smoke
```
