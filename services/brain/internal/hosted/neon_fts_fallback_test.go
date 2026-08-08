package hosted

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestHotLexAvailabilityRequiresUsableIndex(t *testing.T) {
	if (&Client{}).hotLexAvailable() {
		t.Fatal("nil HotLex must be unavailable")
	}
	empty := &Client{hot: NewHotLex("brain")}
	if empty.hotLexAvailable() {
		t.Fatal("empty HotLex must be unavailable")
	}
	empty.hot.AddChunk("chunk", "doc", "bounded lexical evidence", "")
	empty.hot.Finalize()
	if !empty.hotLexAvailable() {
		t.Fatal("non-empty HotLex must be available")
	}
}

func TestMissingHotLexFallbackIsSingleAndTimeBounded(t *testing.T) {
	if got := interactiveFTSQueryCap(false); got != 1 {
		t.Fatalf("missing HotLex query cap=%d want 1", got)
	}
	if got := interactiveFTSQueryCap(true); got != 3 {
		t.Fatalf("available HotLex query cap=%d want existing cap 3", got)
	}

	// Benchmax deliberately removes normal stage deadlines. A missing serving
	// projection must not inherit that unbounded posture for its Neon fallback.
	b := interactiveFTSBudget(ProdProfile{Benchmax: true, Quality: true}, false)
	if b <= 0 || b > 3*time.Second {
		t.Fatalf("missing HotLex benchmax FTS budget=%v want (0,3s]", b)
	}
	if got := interactiveFTSBudget(ProdProfile{Enabled: true, LexTimeout: 1500 * time.Millisecond}, false); got != 1500*time.Millisecond {
		t.Fatalf("tighter product budget=%v want 1.5s", got)
	}
}

func TestCorpusGrepDoesNotRetryNeonWithoutHotLex(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_SKIP_FTS", "0")
	c := &Client{db: &sql.DB{}, cfg: Config{BrainID: "brain"}}
	missingSpent := retrievalFTSState{phaseAFallbackAttempted: true}
	if c.corpusGrepFTSAllowed(missingSpent, 0) {
		t.Fatal("spent missing-HotLex fallback must not allow a corpus-grep Neon retry")
	}
	missingUnspent := retrievalFTSState{}
	if !c.corpusGrepFTSAllowed(missingUnspent, 0) {
		t.Fatal("an unspent missing-HotLex fallback remains available once")
	}
	c.hot = NewHotLex("brain")
	c.hot.AddChunk("chunk", "doc", "Atlas launch evidence", "")
	c.hot.Finalize()
	capturedAvailable := retrievalFTSState{hotLexAvailable: true}
	if !c.corpusGrepFTSAllowed(capturedAvailable, 1) {
		t.Fatal("available but thin HotLex should retain the existing recovery policy")
	}
	// The gate uses the request capture, not a live re-read after the projection
	// pointer changes while the request is in flight.
	c.hot = nil
	if !c.corpusGrepFTSAllowed(capturedAvailable, 1) {
		t.Fatal("live HotLex mutation changed a request-captured recovery decision")
	}
	if c.corpusGrepFTSAllowed(retrievalFTSState{hotLexAvailable: true, ftsDisabled: true}, 1) {
		t.Fatal("explicit FTS opt-out must also cover corpus recovery")
	}
}

func TestMissingHotLexFallbackDiagnosticsAndSpentGate(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_SKIP_DENSE", "1")
	t.Setenv("OUROBOROS_ERB_SKIP_FTS", "0")
	c := &Client{cfg: Config{BrainID: "brain"}}
	_, diag, err := c.retrieveInteractive(
		context.Background(), "bounded fallback", RetrieveOptions{}, map[string]any{},
		8, 24, time.Now(), ProdProfile{Enabled: true, LexTimeout: time.Second}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if diag["hot_lex_available"] != false || diag["hot_lex_missing"] != true {
		t.Fatalf("missing HotLex diagnostics=%v", diag)
	}
	if diag["neon_fts_fallback_query_cap"] != 1 || diag["neon_fts_fallback_attempted"] != false {
		t.Fatalf("fallback diagnostics=%v", diag)
	}
	if missingHotLexFallbackSpent(diag) {
		t.Fatal("no DB/FTS attempt must leave the answer-stage rescue available")
	}
	if missingHotLexFallbackSpent(nil) {
		t.Fatal("missing diagnostics cannot prove the fallback was spent")
	}
	diag["neon_fts_fallback_attempted"] = true
	if !missingHotLexFallbackSpent(diag) {
		t.Fatal("a phase-A attempt must close later missing-HotLex FTS retries")
	}
	diag["hot_lex_state"] = "available"
	if missingHotLexFallbackSpent(diag) {
		t.Fatal("captured available HotLex must retain the event rescue policy")
	}
}
