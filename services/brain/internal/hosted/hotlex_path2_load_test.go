package hosted

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHotLexPath2GobLoadSearch loads a projected path2 gob from the build-spine
// cache when present (env-gate: file created by modal_project_hotlex.py).
func TestHotLexPath2GobLoadSearch(t *testing.T) {
	path := os.Getenv("OUROBOROS_ERB_HOTLEX_PATH")
	if path == "" {
		// repo/tools/build-spine/.cache/hotlex-path2.gob relative to this package
		// services/brain/internal/hosted → ../../../tools/build-spine/.cache
		cand := filepath.Join("..", "..", "..", "tools", "build-spine", ".cache", "hotlex-path2.gob")
		if _, err := os.Stat(cand); err != nil {
			// try absolute from cwd variants
			cand2 := filepath.Join("..", "..", "..", "..", "tools", "build-spine", ".cache", "hotlex-path2.gob")
			if _, err2 := os.Stat(cand2); err2 != nil {
				t.Skip("no path2 hotlex gob; run tools/build-spine/modal_project_hotlex.py")
			}
			cand = cand2
		}
		path = cand
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("hotlex gob missing: %v", err)
	}
	t0 := time.Now()
	h, err := LoadHotLexGob(path)
	loadMs := time.Since(t0).Milliseconds()
	if err != nil {
		t.Fatal(err)
	}
	if h.Len() < 1000 {
		t.Fatalf("docs=%d want >=1000", h.Len())
	}
	t1 := time.Now()
	hits := h.Search("default size limits multipart upload OpenAI", 10)
	searchMs := time.Since(t1).Milliseconds()
	if len(hits) == 0 {
		t.Fatalf("no hits load_ms=%d search_ms=%d docs=%d", loadMs, searchMs, h.Len())
	}
	t.Logf("docs=%d load_ms=%d search_ms=%d top=%s score=%.3f",
		h.Len(), loadMs, searchMs, hits[0].ChunkID, hits[0].Score)
	// Interactive class target: pure BM25 search should be well under 750ms.
	if searchMs > 750 {
		t.Fatalf("hotlex search_ms=%d exceeds 750ms interactive floor", searchMs)
	}
}
