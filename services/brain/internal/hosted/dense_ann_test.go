package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/projections"
)

func TestLocalDenseANNMissingIndexFallbackAndIdentity(t *testing.T) {
	dir := t.TempDir()
	ld, err := openLocalDense(dir, "brain-a")
	if err != nil {
		t.Fatal(err)
	}
	point := DensePoint{ID: "a", Vector: []float32{1, 0, 0}, ModelID: "bag:v1"}
	if err := ld.Upsert([]DensePoint{point}); err != nil {
		t.Fatal(err)
	}
	annPath := ld.annPath
	if err := ld.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(annPath); err != nil {
		t.Fatal(err)
	}
	legacy, err := openLocalDense(dir, "brain-a")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	result, err := legacy.Search(denseQuery{Vector: []float32{1, 0, 0}, ModelID: "bag:v1"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].DSID != "a" ||
		result.Diagnostics.Route != "exact_fallback" || result.Diagnostics.IndexState != "missing" {
		t.Fatalf("missing-index result=%+v", result)
	}
	if result.Diagnostics.ExactFallbackLimit != projections.ExactDenseFallbackLimit {
		t.Fatalf("fallback diagnostics=%+v", result.Diagnostics)
	}

	ready, err := openLocalDense(t.TempDir(), "brain-model")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	if err := ready.Upsert([]DensePoint{point}); err != nil {
		t.Fatal(err)
	}
	_, err = ready.Search(denseQuery{Vector: []float32{1, 0, 0}, ModelID: "bag:v2"}, 1)
	var identityErr *dense.IdentityError
	if !errors.As(err, &identityErr) || identityErr.Field != "model" {
		t.Fatalf("model mismatch error=%v", err)
	}
}

func TestLocalDenseANNMissingIndexFallbackPreservesEvidenceMetadata(t *testing.T) {
	dir := t.TempDir()
	ld, err := openLocalDense(dir, "brain-fallback-meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := ld.Upsert([]DensePoint{{
		ID: "vector-c4", Vector: []float32{1, 0}, ModelID: "bag:v1",
		Payload: map[string]any{"document_id": "doc-4", "chunk_id": "chunk-4", "source_uri": "notion://page/4"},
	}}); err != nil {
		t.Fatal(err)
	}
	annPath := ld.annPath
	if err := ld.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(annPath); err != nil {
		t.Fatal(err)
	}

	reopened, err := openLocalDense(dir, "brain-fallback-meta")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Diagnostics.Route != "exact_fallback" {
		t.Fatalf("result=%+v", result)
	}
	hit := result.Hits[0]
	if hit.DSID != "doc-4" || hit.ChunkID != "chunk-4" || hit.SourceURI != "notion://page/4" {
		t.Fatalf("fallback metadata lost: %+v", hit)
	}
}

func TestLocalDenseANNPreservesACLAndGovernedFilterChokePoint(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "0")
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "0")
	c, err := OpenResidual("brain-filter", SubstrateConfig{
		Dir: t.TempDir(), Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseSQLite,
		Embed: SubstrateAPINone, LLM: SubstrateAPINone, Ranker: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "slack-doc", Text: "bounded ann launch token", SourceURI: "slack://channel/1"},
		{ID: "github-doc", Text: "bounded ann launch token", SourceURI: "github://repo/2"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	c.SetSecurity(productsec.Context{
		Profile: productsec.ProfileMultiPrincipal, Owner: "owner", Principal: "denied",
	})
	denied := c.AnswerOpts(ctx, AnswerOptions{Question: "bounded ann launch token", TopK: 4})
	if denied.Failure != "denied" {
		t.Fatalf("ACL denial=%+v", denied)
	}
	if denied.RetrievalDiagnostics["dense_route"] != nil {
		t.Fatalf("dense ANN ran before ACL denial: %+v", denied.RetrievalDiagnostics)
	}
	c.SetSecurity(productsec.Context{
		Profile: productsec.ProfileMultiPrincipal, Owner: "owner", Principal: "owner",
	})
	c.SetFilterMetadataProvider(func(id string) (DocMeta, bool) {
		switch id {
		case "slack-doc":
			return DocMeta{Tenant: "brain-filter", SourceType: "slack"}, true
		case "github-doc":
			return DocMeta{Tenant: "brain-filter", SourceType: "github"}, true
		default:
			return DocMeta{}, false
		}
	})
	passages, diag, err := c.RetrieveOpts(ctx, "bounded ann launch token", RetrieveOptions{
		TopK: 4, Filter: map[string]any{"source_types": []string{"slack"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		t.Fatalf("filtered passages empty; diagnostics=%+v", diag)
	}
	for _, passage := range passages {
		if passage.DocumentID != "slack-doc" {
			t.Fatalf("filter leaked %q via %q: %+v", passage.DocumentID, passage.Channel, passages)
		}
	}
	if diag["filter_identity"] == nil || diag["dense_route"] == nil {
		t.Fatalf("missing filter/ANN diagnostics: %+v", diag)
	}
	raw, err := json.Marshal(diag)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "gold") {
		t.Fatalf("ANN diagnostics contain gold-derived data: %s", raw)
	}
}

func TestLocalDenseANNSourceScopeDeterminismAndNoGoldDiagnostics(t *testing.T) {
	dir := t.TempDir()
	a, err := openLocalDense(dir, "brain-a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := openLocalDense(dir, "brain-b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, tc := range []struct {
		store *localDense
		id    string
	}{
		{a, "a-only"},
		{b, "b-only"},
	} {
		if err := tc.store.Upsert([]DensePoint{{
			ID: tc.id, Vector: []float32{1, 0}, ModelID: "bag:v1",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		result, err := a.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Hits) != 1 || result.Hits[0].DSID != "a-only" {
			t.Fatalf("attempt %d cross-scope/determinism result=%+v", attempt, result)
		}
		for key := range map[string]any{
			"route": result.Diagnostics.Route, "index_state": result.Diagnostics.IndexState,
			"corpus_vectors": result.Diagnostics.CorpusVectors,
		} {
			if strings.Contains(strings.ToLower(key), "gold") {
				t.Fatalf("gold-derived diagnostic key %q", key)
			}
		}
	}
	if a.annPath == b.annPath {
		t.Fatalf("ANN paths must be source scoped: %q", a.annPath)
	}
}

func TestLocalDenseANNPublishesOnlyAfterDurableSave(t *testing.T) {
	ld, err := openLocalDense(t.TempDir(), "brain-publish")
	if err != nil {
		t.Fatal(err)
	}
	defer ld.Close()
	called := false
	ld.saveANN = func(_ *dense.HNSW, _ string) error {
		called = true
		if ld.ann != nil || ld.indexState != "building" {
			t.Fatalf("ANN exposed before durable publication: ann=%v state=%q", ld.ann != nil, ld.indexState)
		}
		return fmt.Errorf("injected fsync failure")
	}
	err = ld.Upsert([]DensePoint{{ID: "v1", Vector: []float32{1, 0}, ModelID: "bag:v1"}})
	if err == nil || !called {
		t.Fatalf("upsert error=%v save_called=%v", err, called)
	}
	if ld.ann != nil || ld.indexState != "stale" {
		t.Fatalf("failed publication became visible: ann=%v state=%q", ld.ann != nil, ld.indexState)
	}
	result, err := ld.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Diagnostics.Route != "exact_fallback" || result.Diagnostics.IndexState != "stale" {
		t.Fatalf("bounded fallback after failed publication=%+v", result)
	}
}

func TestLocalDenseANNRejectsSameCountContentReplacement(t *testing.T) {
	dir := t.TempDir()
	ld, err := openLocalDense(dir, "brain-digest")
	if err != nil {
		t.Fatal(err)
	}
	points := []DensePoint{
		{ID: "a", Vector: []float32{1, 0}, ModelID: "bag:v1"},
		{ID: "b", Vector: []float32{0, 1}, ModelID: "bag:v1"},
	}
	if err := ld.Upsert(points); err != nil {
		t.Fatal(err)
	}
	if err := ld.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := projections.Open(filepath.Join(dir, "dense.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := &projections.SQLDenseStore{DB: db.SQL}
	identity := dense.IndexIdentity{Scope: "brain-digest", Model: "bag:v1", Dimensions: 2}
	// Replace content while keeping the count at two: the old count-only check
	// would incorrectly serve the stale ANN.
	if err := store.UpsertScoped(identity, "a", []float32{0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openLocalDense(dir, "brain-digest")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.ann != nil || reopened.indexState != "incompatible" {
		t.Fatalf("same-count stale ANN accepted: ann=%v state=%q", reopened.ann != nil, reopened.indexState)
	}
	result, err := reopened.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.Route != "exact_fallback" || result.Diagnostics.IndexState != "incompatible" {
		t.Fatalf("stale digest fallback=%+v", result)
	}
}

func TestLocalDenseANNExplicitModeAndEvidenceMetadata(t *testing.T) {
	for _, tc := range []struct {
		mode  dense.SearchMode
		route string
	}{
		{dense.SearchModeExact, "exact_override"}, {dense.SearchModeANN, "ann_override"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			ld, err := openLocalDense(t.TempDir(), "brain-mode", tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			defer ld.Close()
			if err := ld.Upsert([]DensePoint{{
				ID: "vector-chunk-7", Vector: []float32{1, 0}, ModelID: "bag:v1",
				Payload: map[string]any{"document_id": "doc-7", "chunk_id": "chunk-7", "source_uri": "slack://C7/42"},
			}}); err != nil {
				t.Fatal(err)
			}
			result, err := ld.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 1)
			if err != nil {
				t.Fatal(err)
			}
			if result.Diagnostics.Route != tc.route || len(result.Hits) != 1 {
				t.Fatalf("mode result=%+v", result)
			}
			hit := result.Hits[0]
			if hit.DSID != "doc-7" || hit.ChunkID != "chunk-7" || hit.SourceURI != "slack://C7/42" {
				t.Fatalf("evidence metadata lost: %+v", hit)
			}
		})
	}
}

func TestHNSWDenseEvidenceMetadataSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h, err := openHNSWDense(dir, "brain-hnsw", dense.SearchModeANN)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Upsert([]DensePoint{{
		ID: "vector-9", Vector: []float32{1, 0}, ModelID: "bag:v1",
		Payload: map[string]any{"document_id": "doc-9", "chunk_id": "chunk-9", "source_uri": "github://repo/path#L9"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openHNSWDense(dir, "brain-hnsw", dense.SearchModeANN)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits=%+v", result.Hits)
	}
	hit := result.Hits[0]
	if hit.DSID != "doc-9" || hit.ChunkID != "chunk-9" || hit.SourceURI != "github://repo/path#L9" {
		t.Fatalf("restarted HNSW metadata lost: %+v", hit)
	}
}

func TestHNSWDenseSearchWithoutBuiltIndexReturnsMissing(t *testing.T) {
	h, err := openHNSWDense(t.TempDir(), "brain-empty")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	result, err := h.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 1)
	if err != nil {
		t.Fatalf("Search returned an error for a missing index: %v", err)
	}
	if len(result.Hits) != 0 || result.Diagnostics != denseMissingDiagnostics("missing") {
		t.Fatalf("missing-index result=%+v", result)
	}
}

func TestHNSWDenseUpsertRepairsLegacyEmptyIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dense.hnsw")
	legacy := dense.NewHNSW(2, 16, 64)
	if err := legacy.Upsert("legacy-vector", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if got := legacy.Identity(); got.Scope != "" || got.Model != "" {
		t.Fatalf("test index is not legacy-shaped: %+v", got)
	}
	if err := legacy.Save(path); err != nil {
		t.Fatal(err)
	}

	h, err := openHNSWDense(dir, "brain-legacy", dense.SearchModeExact)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Upsert([]DensePoint{{
		ID: "new-vector", Vector: []float32{0, 1}, ModelID: "bag:v1",
	}}); err != nil {
		t.Fatalf("repairing legacy identity on upsert: %v", err)
	}
	if got, want := h.idx.Identity(), (dense.IndexIdentity{Scope: "brain-legacy", Model: "bag:v1", Dimensions: 2}); got != want {
		t.Fatalf("repaired identity=%+v want %+v", got, want)
	}
	result, err := h.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 2 || result.Hits[0].DSID != "legacy-vector" {
		t.Fatalf("legacy vectors were not preserved after repair: %+v", result)
	}

	reloaded, err := dense.LoadHNSW(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reloaded.Identity(), (dense.IndexIdentity{Scope: "brain-legacy", Model: "bag:v1", Dimensions: 2}); got != want {
		t.Fatalf("persisted repaired identity=%+v want %+v", got, want)
	}
}

type annRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn annRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestFAISSDenseRoundTripsEvidenceMetadata(t *testing.T) {
	f := openFAISSDense("http://faiss.invalid")
	f.client = &http.Client{Transport: annRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/upsert" {
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"chunk-faiss", "doc-faiss", "box://file/1"} {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("upsert dropped %q: %s", want, raw)
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		}
		body := `{"hits":[{"id":"vector-faiss","score":0.9,"payload":{"document_id":"doc-faiss","chunk_id":"chunk-faiss","source_uri":"box://file/1"}}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	payload := map[string]any{"document_id": "doc-faiss", "chunk_id": "chunk-faiss", "source_uri": "box://file/1"}
	if err := f.Upsert([]DensePoint{{ID: "vector-faiss", Vector: []float32{1, 0}, Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	result, err := f.Search(denseQuery{Vector: []float32{1, 0}, ModelID: "bag:v1"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits=%+v", result.Hits)
	}
	hit := result.Hits[0]
	if hit.DSID != "doc-faiss" || hit.ChunkID != "chunk-faiss" || hit.SourceURI != "box://file/1" {
		t.Fatalf("FAISS metadata lost: %+v", hit)
	}
}
