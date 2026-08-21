package contentprivacy

import (
	"sort"
	"sync"
)

// Tombstone() is documented as "the retained non-content authority blocking
// resurrection", and it was a Go map.
//
// A restart dropped every tombstone, so content that had been erased could be
// re-ingested the moment the process came back, and the append-only receipt log
// -- the record of what was erased and why -- vanished with it. An erasure
// guarantee that does not survive a restart is not one.
//
// The package stays persistence-neutral, which was a deliberate property: it
// owns the lifecycle, and deployments own their storage. What it gains is a
// port, so a deployment that has durable storage can be held to the guarantee
// rather than only documented as not meeting it.

// StateStore persists the guard's non-content authority: the tombstone set
// that blocks resurrection, and the append-only receipt log.
//
// Implementations must be durable across process restart. A partial write is
// worse than a failed one here: a tombstone that is lost lets erased content
// return, so Append must not report success until the record survives a crash.
type StateStore interface {
	// LoadState returns everything previously appended, in append order.
	LoadState() ([]Tombstone, []Receipt, error)
	// AppendTombstone durably records one tombstone.
	AppendTombstone(Tombstone) error
	// AppendReceipt durably records one receipt.
	AppendReceipt(Receipt) error
}

// MemoryStateStore is the explicit process-lifetime implementation.
//
// It exists so "this deployment does not persist" is a choice a composition
// makes by naming this type, rather than the silent default a nil store would
// be. Its guarantee is stated in its name.
type MemoryStateStore struct {
	mu         sync.Mutex
	tombstones []Tombstone
	receipts   []Receipt
}

// LoadState returns what this process has recorded.
func (s *MemoryStateStore) LoadState() ([]Tombstone, []Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Tombstone(nil), s.tombstones...), append([]Receipt(nil), s.receipts...), nil
}

// AppendTombstone records one tombstone for this process's lifetime.
func (s *MemoryStateStore) AppendTombstone(stone Tombstone) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tombstones = append(s.tombstones, stone)
	return nil
}

// AppendReceipt records one receipt for this process's lifetime.
func (s *MemoryStateStore) AppendReceipt(receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, receipt)
	return nil
}

// restoreLocked seeds a guard from its store. Tombstones are authoritative and
// are restored first, so a resurrection attempt during restore is still denied.
//
// The receipt sequence resumes from the highest recorded Seq: restarting it at
// zero would make two different receipts share a sequence number, which is the
// one thing an append-only log must not do.
func (g *Guard) restoreLocked(tombstones []Tombstone, receipts []Receipt) {
	for _, stone := range tombstones {
		g.tombstones[encodeKey(stone.TenantID, stone.ScopeKey, stone.ContentID)] = stone
	}
	sorted := append([]Receipt(nil), receipts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	g.receipts = sorted
	for _, receipt := range sorted {
		if receipt.Seq > g.seq {
			g.seq = receipt.Seq
		}
	}
}
