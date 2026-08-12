# Factual-consistency scoring

Status for issue #313: **contract, bounded scoring seam, and hosted calibration
wiring implemented; no production certification is claimed.** Grounded-answer
contracts always expose one `FactualConsistencyScore`, but a numeric score is
valid only
when its status is `SCORED` and it carries an exact scorer version, calibration
ID, and SHA-256 calibration-artifact digest.

## Semantics

- `scored` is numeric in `[0,1000]`. Every answer claim was evaluated and the
  result pins `scorer_id`, `scorer_version`, `calibration_id`, and
  `calibration_digest`.
- `abstained` is not zero confidence. The answer made no claims, the score is
  structurally zero, and the public reason is `answer_abstained`.
- `unknown` is not zero confidence. Missing, failed, timed-out, malformed, or
  over-budget scoring returns no provenance and one bounded public reason.
  Provider errors, evidence identities, and authorization facts never enter the
  reason.

`Evaluate` accepts only claims that the query engine has retained after
citation verification and the exact support spans resolved from those
citations. It receives no principal, tenant, source, revision, ACL, or evidence
identifier. Scoring cannot add prose, claims, or citations. The normal final
emit authorization and propagation-receipt admission still run after scoring,
so revocation removes the complete answer and returns the normal abstention
shape.

The hard ceiling is 64 claims, 16 supports per claim, 4 KiB per statement or
support, 64 KiB total input, and a 50 ms cooperative deadline. Smaller caller
limits are accepted; larger or malformed inputs become `budget_exceeded`.
Scorer errors, panics, deadline breaches, and invalid output become `unknown`.
Caller cancellation still cancels the whole answer.

## Calibration boundary

`LexicalScorer` is a deterministic offline calibration harness. It measures the
mean unique-token overlap between each claim and its cited spans, then maps the
raw statistic through caller-supplied monotonic calibration bins. Construction
requires the canonical calibration digest, so floating or silently modified
bins cannot score.

The hosted answer boundary configures `fc-synthetic-nonofficial-v1`. Its four
Laplace-smoothed bins and 0.778 serving threshold are derived from 30 `fit`
rows under a declared 3:1 false-grounded:false-abstention loss. Sixteen separate
`holdout` rows are loaded only after fitting and reproduce Brier 0.10807675,
ECE 0.10975, accepted-ungrounded rate 0.125, and abstention precision/recall
0.875/0.875. The artifact digest binds the bins and threshold to the exact
dataset SHA-256. The regression derives each raw observation from its checked-in
claim/support text before it separates fit from holdout. Serving embeds only
the fitted values and provenance, never case labels or text.

The data are repository-authored synthetic claim/support pairs, not an
official benchmark, customer sample, or independent external evaluation.
Token overlap is not semantic entailment. The calibrated score therefore never
relaxes hosted quote, concrete-atom, citation, company-evidence, ACL, scope, or
abstention checks. It can only send an otherwise supported answer through the
single existing repair opportunity or force a fail-closed abstention. Other
contract surfaces remain explicitly `scorer_unavailable` until they have a
population-appropriate calibration and the same low-confidence enforcement.

The retained receipt and its limitations are recorded in the stage-09
evidence file `factual-consistency-calibration-v1.json` (not checked into
this standalone extraction).

## Verification

```sh
go test ./services/brain/internal/factualconsistency ./services/brain/internal/query
go test ./services/brain/internal/hosted -run 'Test(AnswerFaithfulness|FactualConsistency|FinalizeExtractiveAnswer)'
buf lint packages/contracts/proto
```
