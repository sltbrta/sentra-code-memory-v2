package productsearch

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

// Search runs multi-arm retrieval for the selected profile (no LLM synth).
func Search(ctx context.Context, req Request) Result {
	t0 := time.Now()
	req = normalize(req)
	switch req.Profile {
	case ProfileCode:
		return finish(searchCode(req), t0)
	case ProfileCodeExact:
		return finish(searchCodeExact(ctx, req), t0)
	case ProfileLocal, ProfileMemory: // Memory is alias for local FS projection
		return finish(searchLocal(ctx, req), t0)
	case ProfileHosted:
		return finish(searchHosted(ctx, req), t0)
	default:
		return finish(Result{Failure: "productsearch: unknown profile", ProductOwned: true}, t0)
	}
}

// Ask is Search + extractive (local/code) or hosted Answer (LLM when keyed).
func Ask(ctx context.Context, req Request) Result {
	t0 := time.Now()
	req = normalize(req)
	if req.Profile == ProfileHosted || req.Hosted {
		r := askHosted(ctx, req)
		return finish(r, t0)
	}
	if req.Profile == ProfileLocal || req.Profile == ProfileMemory {
		r := askLocal(ctx, req)
		return finish(r, t0)
	}
	// code + code_exact: extractive answer from search hits
	r := Search(ctx, req)
	if r.Failure != "" {
		return finish(r, t0)
	}
	// Extractive answer from top hits (code).
	var b strings.Builder
	b.WriteString("Based on product search evidence:\n")
	cites := make([]string, 0, 3)
	for i, h := range r.Hits {
		if i >= 3 {
			break
		}
		title := h.Title
		if title == "" {
			title = h.ID
		}
		snippet := h.Text
		if len(snippet) > 280 {
			snippet = snippet[:280] + "…"
		}
		fmt.Fprintf(&b, "- [%s] %s — %s\n", h.ID, title, snippet)
		cites = append(cites, h.ID)
	}
	r.Answer = strings.TrimSpace(b.String())
	r.CitedIDs = cites
	if r.SearchMode == "" {
		r.SearchMode = "product_search_" + string(req.Profile)
	}
	return finish(r, t0)
}

func normalize(req Request) Request {
	if req.TopK <= 0 {
		req.TopK = 8
	}
	if req.Workers <= 0 {
		req.Workers = 4
	}
	if req.BrainID == "" {
		req.BrainID = "local"
	}
	if req.Profile == "" || req.Profile == ProfileAuto {
		// Prefer hosted residual when remote substrates are configured (BYOK/BYOC).
		// Code workspace crawl is still local; company residual defaults hosted-first.
		if req.Hosted || hosted.Enabled() {
			req.Profile = ProfileHosted
		} else if strings.TrimSpace(req.MemoryDir) != "" {
			req.Profile = ProfileLocal
		} else if strings.TrimSpace(req.CodeRoot) != "" {
			req.Profile = ProfileCode
		} else {
			req.Profile = ProfileLocal
		}
	}
	// Legacy alias
	if req.Profile == ProfileMemory {
		req.Profile = ProfileLocal
	}
	return req
}

func finish(r Result, t0 time.Time) Result {
	r.DurationMs = time.Since(t0).Milliseconds()
	// Product facade defaults product_owned; path2_eval is the explicit exception.
	if r.Guarantee == GuaranteePath2Eval {
		r.ProductOwned = false
	} else {
		r.ProductOwned = true
	}
	return r
}

// hostedPlaneLabels gates residual vs path2_eval from Client.ProductOwned().
// OpenLocal / OpenResidual → residual + residual_company_rag.
// OpenFromEnv (SMF path2) → path2_eval — never residual_company_rag.
func hostedPlaneLabels(productOwned bool) (plane, guarantee string) {
	if productOwned {
		return "residual", GuaranteeResidualCompany
	}
	return "path2_eval", GuaranteePath2Eval
}

// stampHostedHonesty stamps plane/guarantee/product_owned plus mutual-exclusion
// honesty flags on hosted Retrieve/Answer diagnostics (success and error paths).
func stampHostedHonesty(diag map[string]any, productOwned bool) map[string]any {
	if diag == nil {
		diag = map[string]any{}
	}
	plane, guarantee := hostedPlaneLabels(productOwned)
	diag["plane"] = plane
	diag["guarantee"] = guarantee
	diag["product_owned"] = productOwned
	if productOwned {
		diag["not_path2_smf"] = true
		diag["not_authority_query"] = true
	} else {
		diag["not_residual_company"] = true
		diag["path2_smf"] = true
	}
	return diag
}

func searchCode(req Request) Result {
	if strings.TrimSpace(req.CodeRoot) == "" {
		return Result{Failure: "productsearch: code root required", Profile: ProfileCode, SearchMode: "product_search_code"}
	}
	rootAbs, err := filepath.Abs(req.CodeRoot)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileCode, SearchMode: "product_search_code"}
	}
	gobPath := strings.TrimSpace(req.CodeIndexPath)
	if gobPath == "" {
		gobPath = filepath.Join(rootAbs, ".ouroboros", codecrawl.DefaultIndexFile)
	}
	idx, st, wrote, meta, err := codecrawl.OpenOrRefresh(rootAbs, gobPath, req.Workers, req.ForceFullCode)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileCode, SearchMode: "product_search_code"}
	}
	hits := idx.SearchOpts(req.Question, req.TopK, req.SymbolHop)
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		arm := "lexical"
		if h.Score < 1 {
			arm = "symbol_hop"
		}
		out = append(out, Hit{ID: h.Path, Score: h.Score, Arm: arm, Text: h.Path})
	}
	defs, refs := idx.SymbolStats()
	return Result{
		Hits:       out,
		Profile:    ProfileCode,
		SearchMode: "product_search_code",
		Guarantee:  GuaranteeHeuristicWorkspace,
		RetrievalDiagnostics: map[string]any{
			"arms":             []string{"lexical", "symbol_hop"},
			"guarantee":        GuaranteeHeuristicWorkspace,
			"not_code_exact":   true,
			"plane":            "code_operator",
			"symbol_hop":       req.SymbolHop,
			"symbol_defs":      defs,
			"symbol_refs":      refs,
			"files_indexed":    st.FilesIndexed,
			"workers":          st.Workers,
			"crawl_ms":         st.Duration.Milliseconds(),
			"bytes_read":       st.BytesRead,
			"skipped_by_stamp": st.SkippedByStamp,
			"changed":          st.Changed,
			"unchanged":        st.Unchanged,
			"gob_wrote":        wrote,
			"gob_path":         gobPath,
			"schema":           meta.Schema,
			"giant_search":     true,
			"store":            "codecrawl_gob",
		},
	}
}

func openLocal(req Request) (*hosted.Client, error) {
	if strings.TrimSpace(req.MemoryDir) == "" {
		return nil, fmt.Errorf("productsearch: local dir required")
	}
	return hosted.OpenLocal(req.MemoryDir, req.BrainID)
}

func searchLocal(ctx context.Context, req Request) Result {
	c, err := openLocal(req)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileLocal, SearchMode: "product_search_local"}
	}
	defer c.Close()
	passages, diag, err := c.Retrieve(ctx, req.Question, req.TopK)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileLocal, SearchMode: "product_search_local", RetrievalDiagnostics: diag}
	}
	out := make([]Hit, 0, len(passages))
	cites := make([]string, 0, len(passages))
	for _, p := range passages {
		out = append(out, Hit{ID: p.DocumentID, Text: p.Text, Score: p.Score, Arm: "hybrid"})
		cites = append(cites, p.DocumentID)
	}
	if diag == nil {
		diag = map[string]any{}
	}
	diag["giant_search"] = true
	if _, ok := diag["arms"]; !ok {
		diag["arms"] = []string{"lexical", "structure", "facts", "coverage"}
	}
	diag["store"] = c.StoreKind()
	diag["plane"] = "residual"
	diag["guarantee"] = GuaranteeResidualCompany
	return Result{
		Hits:                 out,
		CitedIDs:             cites,
		Profile:              ProfileLocal,
		SearchMode:           "product_search_local",
		Guarantee:            GuaranteeResidualCompany,
		RetrievalDiagnostics: diag,
	}
}

func askLocal(ctx context.Context, req Request) Result {
	c, err := openLocal(req)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileLocal, SearchMode: "product_search_local"}
	}
	defer c.Close()
	ans := c.AnswerOpts(ctx, hosted.AnswerOptions{
		Question:     req.Question,
		TopK:         req.TopK,
		SessionID:    req.SessionID,
		SourceTypes:  req.SourceTypes,
		QuestionType: "",
	})
	hits := make([]Hit, 0, len(ans.CitedDocumentIDs))
	for _, id := range ans.CitedDocumentIDs {
		hits = append(hits, Hit{ID: id, Arm: "local"})
	}
	diag := ans.RetrievalDiagnostics
	if diag == nil {
		diag = map[string]any{}
	}
	diag["store"] = c.StoreKind()
	diag["plane"] = "residual"
	diag["guarantee"] = GuaranteeResidualCompany
	if g := c.GenerationID(); g != "" {
		diag["generation_id"] = g
	}
	return Result{
		Answer:               ans.Answer,
		Hits:                 hits,
		CitedIDs:             ans.CitedDocumentIDs,
		Profile:              ProfileLocal,
		SearchMode:           "product_search_local",
		Guarantee:            GuaranteeResidualCompany,
		Failure:              ans.Failure,
		RetrievalDiagnostics: diag,
	}
}

// openHosted respects Request.BrainID when set (override env brain).
func openHosted(req Request) (*hosted.Client, error) {
	c, err := hosted.OpenFromEnv()
	if err != nil {
		return nil, err
	}
	if id := strings.TrimSpace(req.BrainID); id != "" && id != "local" {
		c = c.WithBrainID(id)
	}
	return c, nil
}

func searchHosted(ctx context.Context, req Request) Result {
	if !hosted.Enabled() {
		return Result{Failure: "productsearch: hosted env not configured", Profile: ProfileHosted, SearchMode: "product_search_hosted"}
	}
	c, err := openHosted(req)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileHosted, SearchMode: "product_search_hosted"}
	}
	defer c.Close()
	productOwned := c.ProductOwned()
	_, guarantee := hostedPlaneLabels(productOwned)
	passages, diag, err := c.Retrieve(ctx, req.Question, req.TopK)
	if err != nil {
		return Result{
			Failure: err.Error(), Profile: ProfileHosted, SearchMode: "product_search_hosted",
			Guarantee: guarantee, RetrievalDiagnostics: stampHostedHonesty(diag, productOwned),
		}
	}
	out := make([]Hit, 0, len(passages))
	cites := make([]string, 0, len(passages))
	for _, p := range passages {
		out = append(out, Hit{ID: p.DocumentID, Text: p.Text, Score: p.Score, Arm: "hosted"})
		cites = append(cites, p.DocumentID)
	}
	if diag == nil {
		diag = map[string]any{}
	}
	diag["giant_search"] = true
	diag["brain_id"] = c.Config().BrainID
	diag["store"] = c.StoreKind()
	if _, ok := diag["arms"]; !ok {
		diag["arms"] = []string{"lexical", "dense", "structure", "facts", "ce", "coverage"}
	}
	// Honesty: never stamp residual_company_rag on OpenFromEnv path2 SMF.
	diag = stampHostedHonesty(diag, productOwned)
	return Result{
		Hits:                 out,
		CitedIDs:             cites,
		Profile:              ProfileHosted,
		SearchMode:           "product_search_hosted",
		Guarantee:            guarantee,
		RetrievalDiagnostics: diag,
	}
}

func askHosted(ctx context.Context, req Request) Result {
	if !hosted.Enabled() {
		return Result{Failure: "productsearch: hosted env not configured", Profile: ProfileHosted, SearchMode: "product_brain_go_hosted"}
	}
	c, err := openHosted(req)
	if err != nil {
		return Result{Failure: err.Error(), Profile: ProfileHosted, SearchMode: "product_brain_go_hosted"}
	}
	defer c.Close()
	productOwned := c.ProductOwned()
	_, guarantee := hostedPlaneLabels(productOwned)
	ans := c.AnswerOpts(ctx, hosted.AnswerOptions{
		Question:    req.Question,
		TopK:        req.TopK,
		SessionID:   req.SessionID,
		SourceTypes: req.SourceTypes,
	})
	hits := make([]Hit, 0, len(ans.CitedDocumentIDs))
	for _, id := range ans.CitedDocumentIDs {
		hits = append(hits, Hit{ID: id, Arm: "hosted"})
	}
	diag := stampHostedHonesty(ans.RetrievalDiagnostics, productOwned)
	return Result{
		Answer:               ans.Answer,
		Hits:                 hits,
		CitedIDs:             ans.CitedDocumentIDs,
		Profile:              ProfileHosted,
		SearchMode:           ans.SearchMode,
		Guarantee:            guarantee,
		Failure:              ans.Failure,
		RetrievalDiagnostics: diag,
	}
}
