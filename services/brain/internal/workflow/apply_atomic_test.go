package workflow_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflow"
)

func TestApplyChangeSetPromotionFailureRestoresEveryFile(t *testing.T) {
	root := t.TempDir()
	beforeA, beforeB := []byte("aaa"), []byte("bbb")
	_ = os.WriteFile(filepath.Join(root, "a.txt"), beforeA, 0o644)
	_ = os.WriteFile(filepath.Join(root, "b.txt"), beforeB, 0o644)
	cs := workflow.ChangeSet{Base: "tree", BaseDigests: map[string]string{"a.txt": workflow.Digest(beforeA), "b.txt": workflow.Digest(beforeB)}, Edits: []workflow.CandidateEdit{
		{Path: "a.txt", Range: workflow.EditRange{Start: 0, End: 3}, Replacement: "AAA", BaseDigest: workflow.Digest(beforeA), PredictedDigest: workflow.Digest([]byte("AAA"))},
		{Path: "b.txt", Range: workflow.EditRange{Start: 0, End: 3}, Replacement: "BBB", BaseDigest: workflow.Digest(beforeB), PredictedDigest: workflow.Digest([]byte("BBB"))},
	}}
	receipt, err := workflow.ApplyChangeSet(context.Background(), root, cs, workflow.ApplyOptions{InjectFailureAt: workflow.FailDuringPromotion})
	if err == nil || receipt.Applied || !receipt.RolledBack {
		t.Fatalf("failure was not rolled back: %v %+v", err, receipt)
	}
	gotA, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	gotB, _ := os.ReadFile(filepath.Join(root, "b.txt"))
	if string(gotA) != "aaa" || string(gotB) != "bbb" {
		t.Fatalf("partial tree: a=%q b=%q", gotA, gotB)
	}
}
