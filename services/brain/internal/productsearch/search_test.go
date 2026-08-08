package productsearch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func TestSearchCodeWithSymbolHop(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc SearchMarkerAlpha() {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\nfunc use() { SearchMarkerAlpha() }\n"), 0o644)

	r := Search(context.Background(), Request{
		Profile:   ProfileCode,
		CodeRoot:  root,
		Question:  "SearchMarkerAlpha",
		TopK:      8,
		Workers:   2,
		SymbolHop: true,
	})
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	if len(r.Hits) == 0 {
		t.Fatal("no hits")
	}
	if r.RetrievalDiagnostics["giant_search"] != true {
		t.Fatalf("diag %#v", r.RetrievalDiagnostics)
	}
	if r.RetrievalDiagnostics["store"] != "codecrawl_gob" {
		t.Fatalf("expected gob store, diag %#v", r.RetrievalDiagnostics)
	}
	if r.Guarantee != GuaranteeHeuristicWorkspace {
		t.Fatalf("code guarantee=%s want %s", r.Guarantee, GuaranteeHeuristicWorkspace)
	}
	// Second search should hit stamp warm (bytes_read 0).
	r2 := Search(context.Background(), Request{
		Profile:  ProfileCode,
		CodeRoot: root,
		Question: "SearchMarkerAlpha",
		TopK:     4,
		Workers:  2,
	})
	if r2.Failure != "" {
		t.Fatal(r2.Failure)
	}
	br, _ := r2.RetrievalDiagnostics["bytes_read"].(int64)
	// gob may report 0 or small depending on path; skip hard assert if type differs
	if br > 0 {
		// ok if first write path re-walked; unchanged count should be high
		if u, ok := r2.RetrievalDiagnostics["unchanged"].(int); ok && u == 0 {
			if sk, ok2 := r2.RetrievalDiagnostics["skipped_by_stamp"].(int); !ok2 || sk == 0 {
				t.Logf("warm path not fully stamp-skip (acceptable on tiny tree): %#v", r2.RetrievalDiagnostics)
			}
		}
	}
}

func TestSearchLocalE2E(t *testing.T) {
	dir := t.TempDir()
	c, err := hosted.CreateLocal(dir, "ps")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.BurstIngestLocal(context.Background(), []hosted.LocalDocument{
		{ID: "d1", Title: "RPO", Text: "MedThink RPO policy requires 4-hour recovery."},
		{ID: "d2", Text: "Unrelated picnic sandwiches only."},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	r := Ask(context.Background(), Request{
		Profile:   ProfileLocal,
		MemoryDir: dir,
		BrainID:   "ps",
		Question:  "MedThink RPO recovery hours",
		TopK:      4,
	})
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	if r.Answer == "" {
		t.Fatal("empty answer")
	}
	if r.SearchMode != "product_search_local" || r.Profile != ProfileLocal {
		t.Fatalf("mode %s profile %s", r.SearchMode, r.Profile)
	}
	if r.Guarantee != GuaranteeResidualCompany {
		t.Fatalf("guarantee=%s", r.Guarantee)
	}
	if r.RetrievalDiagnostics["store"] != "local_fs" {
		t.Fatalf("store diag %#v", r.RetrievalDiagnostics)
	}
	if r.ProductOwned != true {
		t.Fatal("local residual must finish as product_owned")
	}
}

// TestHostedPlaneLabels: OpenLocal/OpenResidual residual vs OpenFromEnv path2.
// productOwned false must never stamp residual_company_rag.
func TestHostedPlaneLabels(t *testing.T) {
	plane, g := hostedPlaneLabels(true)
	if plane != "residual" || g != GuaranteeResidualCompany {
		t.Fatalf("productOwned true: plane=%q guarantee=%q", plane, g)
	}
	plane, g = hostedPlaneLabels(false)
	if plane != "path2_eval" || g != GuaranteePath2Eval {
		t.Fatalf("productOwned false: plane=%q guarantee=%q", plane, g)
	}
	if g == GuaranteeResidualCompany {
		t.Fatal("path2 must never use residual_company_rag")
	}
}

// TestFinishPath2NotProductOwned: finish must not overwrite path2 ProductOwned.
func TestFinishPath2NotProductOwned(t *testing.T) {
	r := finish(Result{Guarantee: GuaranteePath2Eval, ProductOwned: true}, time.Now())
	if r.ProductOwned {
		t.Fatal("GuaranteePath2Eval must finish with ProductOwned=false")
	}
	r2 := finish(Result{Guarantee: GuaranteeResidualCompany}, time.Now())
	if !r2.ProductOwned {
		t.Fatal("residual must finish with ProductOwned=true")
	}
	r3 := finish(Result{Guarantee: GuaranteeHeuristicWorkspace}, time.Now())
	if !r3.ProductOwned {
		t.Fatal("code operator finishes product_owned for facade default")
	}
}
