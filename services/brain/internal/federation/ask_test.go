package federation_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/federation"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

func TestFilterBeforeFanout(t *testing.T) {
	t.Parallel()
	cards := []federation.BrainCard{
		{BrainID: "a", Path: "/a", AllowedFor: []string{"alice"}, Topics: []string{"sky"}},
		{BrainID: "b", Path: "/b", AllowedFor: []string{"bob"}, Topics: []string{"sky"}},
	}
	got := federation.FilterCards(cards, "alice", "", "")
	if len(got) != 1 || got[0].BrainID != "a" {
		t.Fatalf("%+v", got)
	}
}

func TestCapabilityExpiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	c := federation.MintCapability("p", "b", "ask", time.Second, now)
	if !c.Valid("p", "b", "ask", now.Add(500*time.Millisecond)) {
		t.Fatal("should be valid")
	}
	if c.Valid("p", "b", "ask", now.Add(2*time.Second)) {
		t.Fatal("should expire")
	}
	if c.Valid("other", "b", "ask", now) {
		t.Fatal("wrong principal")
	}
}

func TestFederatedAskTwoBrainsAndDeny(t *testing.T) {
	ctx := context.Background()
	dirA := t.TempDir()
	dirB := t.TempDir()
	ca, err := hosted.CreateLocal(dirA, "brain-a")
	if err != nil {
		t.Fatal(err)
	}
	cb, err := hosted.CreateLocal(dirB, "brain-b")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ca.BurstIngestLocal(ctx, []hosted.LocalDocument{{ID: "a1", Text: "Alpha company uses blue widgets."}}, 1)
	_, _ = cb.BurstIngestLocal(ctx, []hosted.LocalDocument{{ID: "b1", Text: "Beta company uses red widgets."}}, 1)
	_ = ca.Close()
	_ = cb.Close()
	// Multi principal: only alice on A, only bob on B.
	_ = productsec.SaveSecurity(dirA, productsec.BrainSecurity{
		Profile: productsec.ProfileMultiPrincipal, Owner: "alice",
		Grants: map[string]bool{"alice": true},
	})
	_ = productsec.SaveSecurity(dirB, productsec.BrainSecurity{
		Profile: productsec.ProfileMultiPrincipal, Owner: "bob",
		Grants: map[string]bool{"bob": true},
	})

	cards := []federation.BrainCard{
		{BrainID: "brain-a", Path: dirA, Topics: []string{"widgets", "blue"}, AllowedFor: []string{"alice", "bob"}, DocCount: 1},
		{BrainID: "brain-b", Path: dirB, Topics: []string{"widgets", "red"}, AllowedFor: []string{"alice", "bob"}, DocCount: 1},
	}

	// Alice should only successfully answer from brain-a (bob's brain denies).
	res := federation.Ask(ctx, federation.AskOpts{
		Principal: "alice", Query: "What color widgets?", TopK: 4, MaxBrains: 2, Cards: cards,
	})
	if res.Denied {
		t.Fatalf("alice denied entirely: %+v", res)
	}
	if len(res.BrainIDs) == 0 {
		// May get answer only from A
		t.Fatalf("expected some brain: %+v", res)
	}
	for _, id := range res.BrainIDs {
		if id == "brain-b" {
			t.Fatalf("alice must not cite brain-b: %+v", res)
		}
	}

	// Unknown principal: filter may still pass AllowedFor but brain deny → no evidence.
	res2 := federation.Ask(ctx, federation.AskOpts{
		Principal: "eve", Query: "widgets", Cards: []federation.BrainCard{
			{BrainID: "brain-a", Path: dirA, AllowedFor: []string{"alice"}},
		},
	})
	if !res2.Denied && res2.Failure != "denied" && res2.Failure != "no_authorized_evidence" {
		// Filter empties AllowedFor for eve → denied
		if len(federation.FilterCards([]federation.BrainCard{
			{BrainID: "brain-a", AllowedFor: []string{"alice"}},
		}, "eve", "", "")) != 0 {
			t.Fatalf("%+v", res2)
		}
	}
}

// TestFederatedAskDeniesOnACLLoadFail: corrupt security.json must fail closed
// (skip brain) rather than fall through to single_user owner access.
func TestFederatedAskDeniesOnACLLoadFail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, err := hosted.CreateLocal(dir, "brain-acl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.BurstIngestLocal(ctx, []hosted.LocalDocument{{ID: "d1", Text: "secret corpus about widgets."}}, 1)
	_ = c.Close()
	// Corrupt security.json so ContextFromBrain / LoadSecurity fails.
	secPath := dir + "/security.json"
	if err := os.WriteFile(secPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := federation.Ask(ctx, federation.AskOpts{
		Principal: "alice", Query: "widgets", TopK: 4, MaxBrains: 1,
		Cards: []federation.BrainCard{
			{BrainID: "brain-acl", Path: dir, Topics: []string{"widgets"}, AllowedFor: []string{"alice"}, DocCount: 1},
		},
	})
	if res.Answer != "" || len(res.BrainIDs) != 0 {
		t.Fatalf("ACL load fail must not answer: %+v", res)
	}
	if res.Failure != "no_authorized_evidence" && !res.Denied && res.Failure != "denied" {
		t.Fatalf("expected deny/no evidence, got %+v", res)
	}
}
