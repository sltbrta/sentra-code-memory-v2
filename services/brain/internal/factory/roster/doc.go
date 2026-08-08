// Package roster owns the durable Stage 05 leaf lease and leaf-result ledger
// facts on the migration 005 factory tables. It performs no I/O of its own:
// every method runs inside a caller-owned database handle or transaction so the
// composing factory kernel can commit roster facts atomically with run, plan,
// and idempotency facts. The schema enforces one densely advancing fence per
// leaf node and exactly one canonical result per leaf; this package derives
// staleness and replay from those insert-only facts.
package roster
