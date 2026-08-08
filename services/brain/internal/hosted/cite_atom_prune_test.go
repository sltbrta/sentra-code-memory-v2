package hosted

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPruneCitesToAnswerAtomsKeepsSupporting(t *testing.T) {
	ps := []Passage{
		{DocumentID: "good", Text: "Default max_file_size is 10 MiB and max_total_request_size is 50 MiB."},
		{DocumentID: "noise", Text: "Unrelated sprint planning notes about pizza."},
	}
	ans := "The default limits are 10 MiB per file and 50 MiB total request size."
	out := pruneCitesToAnswerAtoms([]string{"good", "noise"}, ans, ps, "basic")
	if len(out) != 1 || out[0] != "good" {
		t.Fatalf("want only good, got %v", out)
	}
}

func TestPruneCitesToAnswerAtomsNeverEmpties(t *testing.T) {
	ps := []Passage{
		{DocumentID: "a", Text: "alpha beta gamma unrelated"},
	}
	// Answer tokens do not appear — still keep best cite.
	out := pruneCitesToAnswerAtoms([]string{"a"}, "Completely different wording with no shared tokens xyz.", ps, "basic")
	if len(out) != 1 {
		t.Fatalf("want fallback cite, got %v", out)
	}
}

func TestOfflineEntityRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/entity-catalog.json"
	cat := &OfflineEntityCatalog{
		BrainID: "b",
		Names:   map[string]string{"acme corp": "Acme Corp"},
		NameToDSIDs: map[string][]string{
			"acme corp": {"dsid_acme"},
		},
	}
	if err := WriteOfflineEntityCatalog(path, cat); err != nil {
		t.Fatal(err)
	}
	// Reset process singleton by reading file directly.
	got, err := readEntityCatalogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Names["acme corp"] != "Acme Corp" {
		t.Fatalf("%+v", got)
	}
	if got.NameToDSIDs["acme corp"][0] != "dsid_acme" {
		t.Fatalf("%+v", got)
	}
}

func TestPruneCitesToAnswerAtomsDropsUnrelatedMulti(t *testing.T) {
	ps := []Passage{
		{DocumentID: "g1", Text: "Project Orion beta launch scheduled for March 2024."},
		{DocumentID: "g2", Text: "Orion beta includes auth module and billing gate."},
		{DocumentID: "noise", Text: "Cafeteria menu Tuesday: pizza and salad."},
	}
	ans := "Orion beta launch is March 2024 and includes auth and billing."
	out := pruneCitesToAnswerAtoms([]string{"g1", "g2", "noise"}, ans, ps, "project_related")
	for _, id := range out {
		if id == "noise" {
			t.Fatalf("noise cite kept: %v", out)
		}
	}
	if len(out) < 1 {
		t.Fatal("expected supporting cites")
	}
}

func TestMergeOfflineEntityStubHydratesPreservesTailPlacement(t *testing.T) {
	pool := []Passage{
		{DocumentID: "d1", Text: "", Channel: "entity_catalog_offline", ChunkID: "d1#entity"},
		{DocumentID: "base", Text: "keep", Channel: "hot_lex"},
		{DocumentID: "d2", Text: "", Channel: "entity_catalog_offline", ChunkID: "d2#entity"},
	}
	got := mergeOfflineEntityStubHydrates(pool, []string{"d1", "d2"}, [][]Passage{
		{{DocumentID: "d1", Text: "hydrated d1", Channel: "entity_catalog_offline"}},
		nil,
	})
	if len(got) != 3 || got[0].DocumentID != "base" || got[1].Text != "hydrated d1" || got[2].DocumentID != "d2" {
		t.Fatalf("unexpected merged pool: %#v", got)
	}
}

func TestHydrateOfflineEntityStubsWithBoundsFanoutAndDeadline(t *testing.T) {
	pool := make([]Passage, 12)
	need := make([]string, 12)
	for i := range need {
		need[i] = fmt.Sprintf("d%d", i)
		pool[i] = Passage{DocumentID: need[i], Channel: "entity_catalog_offline", ChunkID: need[i] + "#entity"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	active, maxActive, completed := 0, 0, 0
	started := time.Now()
	got := hydrateOfflineEntityStubsWith(ctx, Config{}, pool, need,
		func(fetchCtx context.Context, dsid string) ([]Hit, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				active--
				mu.Unlock()
			}()
			select {
			case <-time.After(100 * time.Millisecond):
				mu.Lock()
				completed++
				mu.Unlock()
				return []Hit{{ChunkID: dsid + "#1", Text: "late"}, {ChunkID: dsid + "#2", Text: "late"}}, nil
			case <-fetchCtx.Done():
				return nil, fetchCtx.Err()
			}
		})
	if maxActive > 4 {
		t.Fatalf("fanout=%d exceeds bound=4", maxActive)
	}
	if completed != 0 || time.Since(started) >= 200*time.Millisecond {
		t.Fatalf("deadline was not honored: completed=%d elapsed=%s", completed, time.Since(started))
	}
	if len(got) != len(pool) {
		t.Fatalf("deadline should preserve unresolved stubs: got %d passages", len(got))
	}
}

func TestHydrateOfflineEntityStubsNilSafe(t *testing.T) {
	pool := []Passage{
		{DocumentID: "d1", Text: "hello", Channel: "hot_lex"},
		{DocumentID: "d2", Text: "", Channel: "entity_catalog_offline", ChunkID: "d2#entity"},
	}
	// db nil → unchanged
	got := hydrateOfflineEntityStubs(nil, nil, Config{}, pool, 2)
	if len(got) != 2 {
		t.Fatalf("nil db should pass through, got %d", len(got))
	}
}

func TestContestAnswerWithPackHedge(t *testing.T) {
	ps := []Passage{
		{DocumentID: "m", Text: "Action item A1: add counter metric kvcache.refcount.negative.count when sanitizer detects negative refcounts."},
	}
	g := Grounded{
		Answer:           "While the incident writeup does not state the specific metric name added, it details that action item A1 assigns Marcus.",
		CitedDocumentIDs: []string{"m"},
	}
	out, ok := contestAnswerWithPack(
		"What counter metric was added for negative refcounts?",
		"basic",
		g,
		ps,
	)
	if !ok {
		t.Fatal("expected pack contest to win on hedge + metric in pack")
	}
	if !strings.Contains(strings.ToLower(out.Answer), "kvcache.refcount.negative.count") {
		t.Fatalf("want metric name in answer, got %q", out.Answer)
	}
}
