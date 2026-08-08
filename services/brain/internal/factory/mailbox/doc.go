// Package mailbox owns the durable Stage 05 inter-agent message facts on the
// migration 005 factory tables. Message identities are replay-safe: an exact
// resend of the same message collapses to its original dense per-task sequence,
// and a same-identity send carrying different payload conflicts without
// mutating canonical state. Like the roster package, mailbox performs no I/O of
// its own; every method runs inside a caller-owned database handle or
// transaction so the composing factory kernel commits messages atomically with
// run facts.
package mailbox
