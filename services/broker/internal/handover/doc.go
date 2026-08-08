// Package handover implements the bounded Stage 13 local→Modal leaf handover.
//
// V1 proves one non-effectful leaf transfer with portable checkpoint, one active
// fence, at-least-once execution, exactly one fenced committed completion, and
// cleanup receipts. GPU/customer/Kubernetes/VPS portfolios and effectful live
// migration remain deferred (DEF-012).
package handover
