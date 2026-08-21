package contentprivacy_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

// Tombstone() is documented as "the retained non-content authority blocking
// resurrection", and it was a Go map. A restart dropped every tombstone, so
// erased content could be re-ingested the moment the process came back, and
// the append-only receipt log vanished with it.

func statePolicy() contentprivacy.Policy {
	return contentprivacy.Policy{
		ID: "test-policy", Version: "v1",
		MaxContentBytes: 1 << 20, MaxFindings: 64,
		Scopes: map[contentprivacy.ScopeKind]contentprivacy.ScopePolicy{
			contentprivacy.ScopeCompany: {
				Actions: map[contentprivacy.Class]contentprivacy.Action{
					contentprivacy.ClassAPIKey:      contentprivacy.ActionRedact,
					contentprivacy.ClassEmail:       contentprivacy.ActionRedact,
					contentprivacy.ClassSSN:         contentprivacy.ActionRedact,
					contentprivacy.ClassCreditCard:  contentprivacy.ActionRedact,
					contentprivacy.ClassBearerToken: contentprivacy.ActionRedact,
					contentprivacy.ClassPrivateKey:  contentprivacy.ActionTombstone,
				},
				DetectorFailure: contentprivacy.ActionQuarantine,
				Retention:       time.Hour,
			},
		},
	}
}

func tenantScope() contentprivacy.Scope {
	return contentprivacy.Scope{Kind: contentprivacy.ScopeCompany}
}

func guardOver(t *testing.T, dir string) (*contentprivacy.Guard, func()) {
	t.Helper()
	store, err := contentprivacy.OpenFileStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := contentprivacy.NewWithState(statePolicy(), nil, nil, time.Now, store)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return guard, func() { _ = store.Close() }
}

// TestATombstoneSurvivesARestart is the finding. A restart used to drop every
// tombstone, so content that had been erased could be re-ingested immediately.
func TestATombstoneSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	first, closeFirst := guardOver(t, dir)
	if _, err := first.Tombstone("tenant-1", tenantScope(), "erased-doc", "user_request"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if len(first.Tombstones()) != 1 {
		t.Fatalf("tombstone not recorded: %+v", first.Tombstones())
	}
	closeFirst()

	// A new process over the same directory.
	second, closeSecond := guardOver(t, dir)
	defer closeSecond()

	stones := second.Tombstones()
	if len(stones) != 1 {
		t.Fatalf("the tombstone did not survive the restart: %+v", stones)
	}
	if stones[0].ContentID != "erased-doc" || stones[0].Reason != "user_request" {
		t.Fatalf("restored tombstone is wrong: %+v", stones[0])
	}

	// The point of a tombstone: erased content must not come back.
	decision, err := second.Admit(contentprivacy.Input{
		TenantID: "tenant-1", ID: "erased-doc", Scope: tenantScope(),
		Content: "ordinary text with nothing sensitive in it",
	})
	if err == nil && decision.Status != contentprivacy.StatusTombstoned {
		t.Fatalf("erased content was re-ingested after a restart: %+v (err=%v)", decision, err)
	}
}

// TestTheReceiptLogSurvivesARestart covers the other half: the record of what
// was erased and why is append-only, and vanished with the process.
func TestTheReceiptLogSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	first, closeFirst := guardOver(t, dir)
	if _, err := first.Admit(contentprivacy.Input{
		TenantID: "tenant-1", ID: "doc-1", Scope: tenantScope(),
		Content: "contact alice@example.invalid for details",
	}); err != nil {
		t.Fatal(err)
	}
	before := first.Receipts()
	if len(before) < 2 {
		t.Fatalf("want an install receipt and an admission receipt, got %d", len(before))
	}
	closeFirst()

	second, closeSecond := guardOver(t, dir)
	defer closeSecond()
	after := second.Receipts()
	if len(after) != len(before) {
		t.Fatalf("the receipt log did not survive: %d receipts before, %d after",
			len(before), len(after))
	}
	for i := range before {
		if before[i].Seq != after[i].Seq || before[i].Kind != after[i].Kind {
			t.Fatalf("receipt %d changed across the restart: %+v vs %+v",
				i, before[i], after[i])
		}
	}
}

// TestReceiptSequenceDoesNotRestartAtZero is what an append-only log must not
// do: two different receipts sharing a sequence number.
func TestReceiptSequenceDoesNotRestartAtZero(t *testing.T) {
	dir := t.TempDir()

	first, closeFirst := guardOver(t, dir)
	if _, err := first.Admit(contentprivacy.Input{
		TenantID: "tenant-1", ID: "doc-1", Scope: tenantScope(), Content: "plain text",
	}); err != nil {
		t.Fatal(err)
	}
	highest := uint64(0)
	for _, receipt := range first.Receipts() {
		if receipt.Seq > highest {
			highest = receipt.Seq
		}
	}
	closeFirst()

	second, closeSecond := guardOver(t, dir)
	defer closeSecond()
	if _, err := second.Admit(contentprivacy.Input{
		TenantID: "tenant-1", ID: "doc-2", Scope: tenantScope(), Content: "other text",
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]bool{}
	for _, receipt := range second.Receipts() {
		if seen[receipt.Seq] {
			t.Fatalf("sequence %d appears twice after a restart", receipt.Seq)
		}
		seen[receipt.Seq] = true
		if receipt.Seq > highest {
			highest = receipt.Seq
		}
	}
	if len(seen) < 3 {
		t.Fatalf("want the install plus two admissions, got %d receipts", len(seen))
	}
}

// TestTheStateFilesAreNotWorldReadable: the logs name what a tenant erased.
func TestTheStateFilesAreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	guard, done := guardOver(t, dir)
	if _, err := guard.Tombstone("tenant-1", tenantScope(), "erased-doc", "user_request"); err != nil {
		t.Fatal(err)
	}
	done()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is mode %04o: it names what a tenant erased", entry.Name(), mode)
		}
	}
	if checked == 0 {
		t.Fatal("no state files were written, so this guard checked nothing")
	}
	if _, err := os.Stat(filepath.Join(dir, "content-privacy-tombstones.jsonl")); err != nil {
		t.Fatalf("no tombstone log: %v", err)
	}
}

// TestMemoryStateStoreIsStillAvailableAndNamed keeps the process-lifetime
// choice explicit rather than making it the silent default.
func TestMemoryStateStoreIsStillAvailableAndNamed(t *testing.T) {
	guard, err := contentprivacy.NewWithState(
		statePolicy(), nil, nil, time.Now, &contentprivacy.MemoryStateStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Tombstone("tenant-1", tenantScope(), "doc", "reason"); err != nil {
		t.Fatal(err)
	}
	if len(guard.Tombstones()) != 1 {
		t.Fatal("the in-memory store did not record the tombstone")
	}
}
