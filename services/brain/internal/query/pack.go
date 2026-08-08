package query

import (
	"fmt"
	"sort"
)

// PackOrder selects the document ordering applied within the narrow
// grounding pack. Ordering permutes only survivors; it never changes any
// entry's source offsets (BlockStartLine) or content and never widens the
// wide retrieval pool. The retriever's input order is the rank: index 0 is
// the strongest evidence.
type PackOrder string

const (
	// PackOrderRetrieval keeps retrieval-rank order (best-first). It is the
	// deterministic default and the fallback for an unknown order.
	PackOrderRetrieval PackOrder = "retrieval"
	// PackOrderOriginal restores canonical document order, sorted by path
	// then block start line, independent of retrieval rank.
	PackOrderOriginal PackOrder = "original"
	// PackOrderTailFirst reverses retrieval rank so the strongest evidence is
	// packed last (best-last), an OP-RAG style ordering.
	PackOrderTailFirst PackOrder = "tail_first"
	// PackOrderHeadTail mitigates lost-in-the-middle by placing the strongest
	// evidence at both pack edges and the weakest in the middle: rank 0 at the
	// front, rank 1 at the back, rank 2 second, rank 3 second-to-last, and so
	// on.
	PackOrderHeadTail PackOrder = "head_tail"
)

// PackBudget bounds the narrow grounding pack below the frozen Limits. A zero
// value means "use the frozen limits". TargetEntries and TargetBytes, when
// positive, further narrow the pack; they never exceed the frozen ceilings,
// so an adaptive budget can narrow but never widen beyond the contract.
type PackBudget struct {
	TargetEntries int
	TargetBytes   int
}

// PackPolicy is the adaptive ordering and budget policy for the narrow
// grounding pack. The zero value is the deterministic fallback: retrieval
// order and the frozen pack bounds, byte-identical to the legacy packer.
type PackPolicy struct {
	Order  PackOrder
	Budget PackBudget
}

// validate rejects unknown orders and budgets outside [0, frozen ceiling] so a
// misshapen policy fails at engine construction, never at request time.
func (p PackPolicy) validate(limits Limits) error {
	switch p.Order {
	case "", PackOrderRetrieval, PackOrderOriginal, PackOrderHeadTail, PackOrderTailFirst:
	default:
		return fmt.Errorf("%w: unknown pack order %q", ErrInvalidInput, p.Order)
	}
	if p.Budget.TargetEntries < 0 || p.Budget.TargetEntries > limits.MaxEvidenceEntries {
		return fmt.Errorf("%w: pack target entries out of bounds", ErrInvalidInput)
	}
	if p.Budget.TargetBytes < 0 || p.Budget.TargetBytes > limits.MaxEvidencePackBytes {
		return fmt.Errorf("%w: pack target bytes out of bounds", ErrInvalidInput)
	}
	return nil
}

// bounds resolves the effective entry and byte caps, narrowing the frozen
// limits by any positive target. It never widens past the frozen ceilings.
func (p PackPolicy) bounds(limits Limits) (entryCap, byteCap int) {
	entryCap = limits.MaxEvidenceEntries
	byteCap = limits.MaxEvidencePackBytes
	if p.Budget.TargetEntries > 0 && p.Budget.TargetEntries < entryCap {
		entryCap = p.Budget.TargetEntries
	}
	if p.Budget.TargetBytes > 0 && p.Budget.TargetBytes < byteCap {
		byteCap = p.Budget.TargetBytes
	}
	return entryCap, byteCap
}

// entryBytes measures one evidence entry's hydrated block bytes, including
// the newline separators the citation resolver reconstructs.
func entryBytes(entry EvidenceEntry) int {
	total := 0
	for _, line := range entry.Lines {
		total += len(line) + 1
	}
	return total
}

// orderEvidence returns a permutation of the wide-pool entry indices in the
// strategy's packing order. The input slice is never mutated; offsets, lines,
// and identity are carried untouched so citation anchor math is unaffected by
// ordering. An unknown order falls back to retrieval rank.
func orderEvidence(entries []EvidenceEntry, order PackOrder) []int {
	indices := make([]int, len(entries))
	for i := range indices {
		indices[i] = i
	}
	switch order {
	case PackOrderOriginal:
		sort.SliceStable(indices, func(a, b int) bool {
			ea, eb := entries[indices[a]], entries[indices[b]]
			if ea.Path != eb.Path {
				return ea.Path < eb.Path
			}
			return ea.BlockStartLine < eb.BlockStartLine
		})
	case PackOrderTailFirst:
		for i, j := 0, len(indices)-1; i < j; i, j = i+1, j-1 {
			indices[i], indices[j] = indices[j], indices[i]
		}
	case PackOrderHeadTail:
		edges := make([]int, len(indices))
		lo, hi := 0, len(indices)-1
		for rank, source := range indices {
			if rank%2 == 0 {
				edges[lo] = source
				lo++
			} else {
				edges[hi] = source
				hi--
			}
		}
		indices = edges
	case PackOrderRetrieval:
		// identity
	default:
		// Unknown order: fall back to retrieval rank rather than drop evidence.
	}
	return indices
}

// packEvidence applies the frozen pack bounds in deterministic retrieval-rank
// order. It is the legacy entry point and the deterministic fallback; it is
// byte-identical to packEvidenceWithPolicy with the zero PackPolicy.
func packEvidence(entries []EvidenceEntry, limits Limits) (packed []EvidenceEntry, dropped []EvidenceEntry) {
	return packEvidenceWithPolicy(entries, PackPolicy{}, limits)
}

// packEvidenceWithPolicy narrows the wide retrieval pool (entries, in
// retrieval-rank order) into the narrow grounding pack under one policy.
//
// The wide pool stays separate from the narrow pack: survivors are ordered by
// policy.Order, bounded by the policy budget clamped to the frozen Limits, and
// the remainder is returned as dropped in wide-pool input order so the engine
// can disclose it as partial_coverage. Per-entry source offsets are absolute
// and self-describing, so reordering and truncation never change the bytes a
// citation resolves: a survivor's citation still verifies, and a citation into
// a truncated slot fails verification rather than binding stale bytes.
func packEvidenceWithPolicy(entries []EvidenceEntry, policy PackPolicy, limits Limits) (packed []EvidenceEntry, dropped []EvidenceEntry) {
	entryCap, byteCap := policy.bounds(limits)
	chosen := make([]bool, len(entries))
	total := 0
	for _, index := range orderEvidence(entries, policy.Order) {
		entry := entries[index]
		size := entryBytes(entry)
		if len(packed) >= entryCap || size > limits.MaxEvidenceEntryBytes || total+size > byteCap {
			continue
		}
		total += size
		packed = append(packed, entry)
		chosen[index] = true
	}
	for index, entry := range entries {
		if !chosen[index] {
			dropped = append(dropped, entry)
		}
	}
	return packed, dropped
}
