package hosted_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

func TestMultiPrincipalDenyBeforeRetrieve(t *testing.T) {
	dir := t.TempDir()
	c, err := hosted.CreateLocal(dir, "brain-a")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	docs := []hosted.LocalDocument{{ID: "d1", Text: "The sky is blue on Earth."}}
	if _, err := c.BurstIngestLocal(ctx, docs, 1); err != nil {
		t.Fatal(err)
	}
	_, _ = productsec.UpdateEvidenceDigest(dir)

	// Configure multi_principal owner alice; bob denied.
	sec := productsec.BrainSecurity{
		Profile: productsec.ProfileMultiPrincipal, Owner: "alice", VaultCapable: true,
	}
	if err := productsec.SaveSecurity(dir, sec); err != nil {
		t.Fatal(err)
	}
	c2, err := hosted.OpenLocal(dir, "brain-a")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetSecurity(productsec.Context{
		Profile: productsec.ProfileMultiPrincipal, Owner: "alice", Principal: "bob",
	})
	ans := c2.AnswerOpts(ctx, hosted.AnswerOptions{Question: "What color is the sky?", TopK: 4})
	if ans.Failure != "denied" {
		t.Fatalf("want denied, got %+v", ans)
	}
	// Owner succeeds.
	c2.SetSecurity(productsec.Context{
		Profile: productsec.ProfileMultiPrincipal, Owner: "alice", Principal: "alice",
	})
	ans2 := c2.AnswerOpts(ctx, hosted.AnswerOptions{Question: "What color is the sky?", TopK: 4})
	if ans2.Failure == "denied" {
		t.Fatalf("owner denied: %+v", ans2)
	}
	// Evidence digest stable after ask (gardener non-mutation baseline).
	d1, _ := productsec.DigestFile(filepath.Join(dir, "chunks.jsonl"))
	if d1 == "" {
		t.Fatal("empty digest")
	}
	if _, err := os.Stat(filepath.Join(dir, "security.json")); err != nil {
		t.Fatal(err)
	}
}
