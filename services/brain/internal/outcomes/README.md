# Sanitized outcome admission

Status: **[partial] Stage 06 L2 — admission ledger only.** Admits Tracer 001
outcome facts as **machine observations**. Raw traces stay under their original
restricted scope and never enter the admitted bundle. Model proposals, prompts,
secrets, and raw source fail closed (`OUTCOME_SANITIZATION_FAILED`).

## API

```go
store := outcomes.New()
fact, err := store.Admit(outcomes.AdmitRequest{
    AuthorityClass:    outcomes.AuthorityMachineObservation,
    RawTraceSeparated: true,
    OutcomeBundle:     sanitizedJSON,
    // ...
})
err = outcomes.RetainRawTrace(outcomes.RawTraceRecord{SeparatedFromOutcome: true, ...})
err = outcomes.SanitizeCheck(bundle)
```

## Invariants

| Invariant | Result on violation |
| --- | --- |
| `authority_class == machine_observation` | sanitization failure, nothing admitted |
| `raw_trace_separated == true` | sanitization failure |
| No forbidden keys in bundle | sanitization failure |
| Exact idempotent retry | same receipt, `Replayed=true` |
| Same key, different digest | conflict |

## Tests

```text
bazel test //services/brain/internal/outcomes:outcomes_test
```
