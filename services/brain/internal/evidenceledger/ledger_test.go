package evidenceledger_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestEvidenceIsImmutableIdempotentAndScopedByTenantAndBrain(t *testing.T) {
	t.Parallel()
	ledger, err := evidenceledger.New(evidenceledger.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	record := fixture("tenant-a", "brain-a", "evidence-a", "artifact-a")
	created, err := ledger.Admit(ctx, record)
	if err != nil || !created {
		t.Fatalf("first Admit() = (%v, %v)", created, err)
	}
	created, err = ledger.Admit(ctx, record)
	if err != nil || created {
		t.Fatalf("duplicate Admit() = (%v, %v)", created, err)
	}
	changed := record
	changed.Anchor = "bytes:2-3"
	if _, err := ledger.Admit(ctx, changed); !errors.Is(err, evidenceledger.ErrConflict) {
		t.Fatalf("changed duplicate error = %v", err)
	}
	for _, scope := range [][2]string{{"tenant-b", "brain-a"}, {"tenant-a", "brain-b"}} {
		_, err := ledger.Read(ctx, id("tenant", scope[0]), id("brain", scope[1]), record.Evidence)
		if !errors.Is(err, evidenceledger.ErrNotFound) {
			t.Fatalf("cross-scope Read(%v) error = %v", scope, err)
		}
	}
}

func TestEvidenceRequiresCanonicalSHA256AndCompositeScopesDoNotCollide(t *testing.T) {
	t.Parallel()
	ledger, err := evidenceledger.New(evidenceledger.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, digest := range []string{"", "00", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		record := fixture("tenant-a", "brain-a", "evidence-"+digest, "artifact-a")
		record.Digest.Hex = digest
		if _, err := ledger.Admit(ctx, record); !errors.Is(err, evidenceledger.ErrInvalid) {
			t.Fatalf("digest %q error = %v", digest, err)
		}
	}
	left := fixture("a\x00b", "c", "evidence", "artifact-a")
	right := fixture("a", "b\x00c", "evidence", "artifact-b")
	if _, err := ledger.Admit(ctx, left); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Admit(ctx, right); err != nil {
		t.Fatalf("length-prefixed scope collision: %v", err)
	}
}

func TestEvidenceRejectsOversizedDigestBeforeAdmission(t *testing.T) {
	t.Parallel()
	ledger, err := evidenceledger.New(evidenceledger.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	record := fixture("tenant-a", "brain-a", "evidence-a", "artifact-a")
	record.Digest.Hex = strings.Repeat("0", 1<<20)
	if _, err := ledger.Admit(context.Background(), record); !errors.Is(err, evidenceledger.ErrInvalid) {
		t.Fatalf("oversized digest error = %v", err)
	}
}

func TestLineageCannotCrossBrainAndTombstoneRemovesReadableEdges(t *testing.T) {
	t.Parallel()
	ledger, err := evidenceledger.New(evidenceledger.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parent := fixture("tenant-a", "brain-a", "parent", "artifact-a")
	child := fixture("tenant-a", "brain-a", "child", "artifact-b")
	otherBrain := fixture("tenant-a", "brain-b", "other", "artifact-c")
	for _, record := range []evidenceledger.Record{parent, child, otherBrain} {
		if _, err := ledger.Admit(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	edge := evidenceledger.Lineage{Tenant: parent.Tenant, Brain: parent.Brain, Parent: parent.Evidence, Child: child.Evidence, Relation: "derived-from"}
	if created, err := ledger.Link(ctx, edge); err != nil || !created {
		t.Fatalf("Link() = (%v, %v)", created, err)
	}
	crossBrain := edge
	crossBrain.Child = otherBrain.Evidence
	if _, err := ledger.Link(ctx, crossBrain); !errors.Is(err, evidenceledger.ErrNotFound) {
		t.Fatalf("cross-brain Link error = %v", err)
	}
	if err := ledger.Tombstone(ctx, child.Tenant, child.Brain, child.Evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Read(ctx, child.Tenant, child.Brain, child.Evidence); !errors.Is(err, evidenceledger.ErrNotFound) {
		t.Fatalf("tombstoned Read error = %v", err)
	}
	edges, err := ledger.Related(ctx, parent.Tenant, parent.Brain, parent.Evidence)
	if err != nil || len(edges) != 0 {
		t.Fatalf("Related after tombstone = (%+v, %v)", edges, err)
	}
}

func TestLineageInsertAndTombstoneAreAtomic(t *testing.T) {
	t.Parallel()
	for iteration := range 50 {
		repository := evidenceledger.NewMemoryRepository()
		ledger, err := evidenceledger.New(repository)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		parent := fixture("tenant-a", "brain-a", "parent", "artifact-a")
		child := fixture("tenant-a", "brain-a", "child-"+string(rune(iteration+1)), "artifact-b")
		for _, record := range []evidenceledger.Record{parent, child} {
			if _, err := ledger.Admit(ctx, record); err != nil {
				t.Fatal(err)
			}
		}
		edge := evidenceledger.Lineage{Tenant: parent.Tenant, Brain: parent.Brain, Parent: parent.Evidence, Child: child.Evidence, Relation: "derived-from"}
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var linkErr, tombstoneErr error
		go func() {
			defer wait.Done()
			<-start
			_, linkErr = ledger.Link(ctx, edge)
		}()
		go func() {
			defer wait.Done()
			<-start
			tombstoneErr = ledger.Tombstone(ctx, child.Tenant, child.Brain, child.Evidence)
		}()
		close(start)
		wait.Wait()
		if tombstoneErr != nil || linkErr != nil && !errors.Is(linkErr, evidenceledger.ErrNotFound) {
			t.Fatalf("atomic race errors = link %v, tombstone %v", linkErr, tombstoneErr)
		}
		edges, err := ledger.Related(ctx, parent.Tenant, parent.Brain, parent.Evidence)
		if err != nil || len(edges) != 0 {
			t.Fatalf("orphan lineage after tombstone = (%+v, %v)", edges, err)
		}
	}
}

func fixture(tenant, brain, evidence, artifact string) evidenceledger.Record {
	return evidenceledger.Record{
		Tenant: id("tenant", tenant), Brain: id("brain", brain), Evidence: id("evidence", evidence), Artifact: id("artifact", artifact),
		Generation: 1, Anchor: "bytes:0-1", Digest: contracts.Digest{Algorithm: "sha256", Hex: strings.Repeat("0", 64)},
	}
}

func id(namespace, value string) contracts.Identifier {
	return contracts.Identifier{Namespace: namespace, Value: value}
}
