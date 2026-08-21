package hosted

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

// Wiring redaction into ingest changes indexed text, and therefore ranking.
// The decision to wire it required a before-and-after retrieval-quality run
// rather than a correctness proof alone: redaction that silently destroys
// retrieval is not an acceptable trade for redaction that works.
//
// This is that run, kept as a test so the trade stays measured rather than
// having been measured once. It indexes the same corpus twice -- unguarded and
// guarded -- and compares hit rates over the same probes.

type qualityProbe struct {
	query string
	want  string
}

// qualityCorpus mixes documents that carry sensitive spans with documents that
// do not, because the interesting question is what redaction costs the
// *surrounding* text. A corpus of nothing but secrets would measure nothing.
func qualityCorpus() []LocalDocument {
	docs := []LocalDocument{
		{ID: "deploy-runbook", Text: "the deploy runbook rotates the release credential " +
			"sk" + "-abcdefghijklmnopqrstuvwxyz012345 before every staging cutover"},
		{ID: "oncall-rota", Text: "the oncall rota escalates to alice@example.invalid " +
			"after fifteen minutes of unacknowledged pager alerts"},
		{ID: "billing-policy", Text: "the billing policy stores no card number " +
			"beyond 4111111111111111 in the reconciliation ledger export"},
		{ID: "revenue-recognition", Text: "quarterly revenue recognition defers " +
			"subscription income across the contract term for enterprise agreements"},
		{ID: "index-design", Text: "the lexical index uses stamp comparison to skip " +
			"unchanged files during a warm refresh of the crawl"},
		{ID: "retention-policy", Text: "the retention policy expires session transcripts " +
			"after ninety days and tombstones them permanently"},
	}
	// Padding, so a hit is a ranking result rather than the only document.
	for i := 0; i < 12; i++ {
		docs = append(docs, LocalDocument{
			ID:   fmt.Sprintf("filler-%02d", i),
			Text: fmt.Sprintf("unrelated engineering note number %d about build tooling", i),
		})
	}
	return docs
}

func qualityProbes() []qualityProbe {
	return []qualityProbe{
		{"deploy runbook staging cutover", "deploy-runbook"},
		{"oncall rota pager escalation", "oncall-rota"},
		{"billing policy reconciliation ledger", "billing-policy"},
		{"quarterly revenue recognition enterprise", "revenue-recognition"},
		{"lexical index warm refresh stamp", "index-design"},
		{"retention policy session transcripts", "retention-policy"},
	}
}

type qualityResult struct {
	hit1, hit3         float64
	redacted, withheld int
}

func measureQuality(t *testing.T, guarded bool) qualityResult {
	t.Helper()
	dir := t.TempDir()
	client, err := OpenLocal(dir, "quality")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if guarded {
		store, err := contentprivacy.OpenFileStateStore(dir + "/privacy")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		guard, err := contentprivacy.NewWithState(redactPolicy(), nil, nil, time.Now, store)
		if err != nil {
			t.Fatal(err)
		}
		client.WithContentPrivacy(guard, contentprivacy.Scope{Kind: contentprivacy.ScopeCompany})
	}

	ctx := context.Background()
	ingest, err := client.BurstIngestLocal(ctx, qualityCorpus(), 2)
	if err != nil {
		t.Fatalf("ingest (guarded=%v): %v", guarded, err)
	}
	out := qualityResult{redacted: ingest.Redacted, withheld: len(ingest.Withheld)}

	var at1, at3, total float64
	for _, probe := range qualityProbes() {
		hits, err := client.store.LexicalSearch(ctx, "quality", probe.query, 3)
		if err != nil {
			t.Fatal(err)
		}
		total++
		for i, hit := range hits {
			if hit.DSID != probe.want {
				continue
			}
			if i == 0 {
				at1++
			}
			at3++
			break
		}
	}
	if total > 0 {
		out.hit1 = at1 / total
		out.hit3 = at3 / total
	}
	return out
}

func TestRedactionDoesNotDestroyRetrieval(t *testing.T) {
	plain := measureQuality(t, false)
	guarded := measureQuality(t, true)

	t.Logf("unguarded hit@1=%.2f hit@3=%.2f", plain.hit1, plain.hit3)
	t.Logf("guarded   hit@1=%.2f hit@3=%.2f redacted=%d withheld=%d",
		guarded.hit1, guarded.hit3, guarded.redacted, guarded.withheld)

	if plain.hit3 == 0 {
		t.Fatal("the unguarded baseline retrieves nothing, so this comparison " +
			"is between two broken configurations")
	}
	if guarded.redacted == 0 {
		t.Fatal("the guarded run redacted nothing, so the two configurations " +
			"are indexing identical text and the comparison is vacuous")
	}
	if guarded.hit3 < plain.hit3 {
		t.Fatalf("redaction cost hit@3: %.2f unguarded, %.2f guarded. Redaction "+
			"removes only the sensitive span; a drop means it is removing the "+
			"surrounding text that made the document findable.",
			plain.hit3, guarded.hit3)
	}
	if guarded.hit1 < plain.hit1 {
		t.Errorf("redaction cost hit@1: %.2f unguarded, %.2f guarded", plain.hit1, guarded.hit1)
	}
}

// TestRedactionLeavesNonSensitiveDocumentsAlone isolates the property the
// comparison above depends on: a document with nothing to redact must index
// byte-identically either way.
func TestRedactionLeavesNonSensitiveDocumentsAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	client, err := OpenLocal(dir, "quality")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := contentprivacy.OpenFileStateStore(dir + "/privacy")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	guard, err := contentprivacy.NewWithState(redactPolicy(), nil, nil, time.Now, store)
	if err != nil {
		t.Fatal(err)
	}
	client.WithContentPrivacy(guard, contentprivacy.Scope{Kind: contentprivacy.ScopeCompany})

	const clean = "the lexical index uses stamp comparison to skip unchanged files"
	result, err := client.BurstIngestLocal(ctx, []LocalDocument{{ID: "clean", Text: clean}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Redacted != 0 {
		t.Fatalf("a document with nothing sensitive was altered: %+v", result)
	}
	if len(result.Withheld) != 0 {
		t.Fatalf("a clean document was withheld: %+v", result.Withheld)
	}
}
