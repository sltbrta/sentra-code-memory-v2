package llmadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultModel is the documented stable Gemini model for this adapter.
const DefaultModel = "gemini-3.6-flash"

// Operation identifies one bounded structured operation.
type Operation string

const (
	OpExpandQuery     Operation = "expand_query"
	OpScoreCandidates Operation = "score_candidates"
	OpExtractClaims   Operation = "extract_claims"
)

// Config bounds the adapter. The zero value is not usable; use DefaultConfig.
type Config struct {
	// APIKey is read from GEMINI_API_KEY. Empty means deterministic local
	// behavior only. It is never logged, persisted, or sent anywhere except
	// the provider client constructor.
	APIKey string
	// Model defaults to DefaultModel; override for tests/operations.
	Model string
	// Timeout is the per-operation budget, always clamped to the caller's
	// remaining deadline minus a reserve.
	Timeout time.Duration
	// MaxInputBytes bounds any single input string before prompting.
	MaxInputBytes int
	// MaxCandidateBytes bounds one candidate's text before prompting.
	MaxCandidateBytes int
	// MaxCandidates bounds the candidate set per scoring call.
	MaxCandidates int
	// MaxExpansions bounds returned query expansions (including the original).
	MaxExpansions int
	// MaxClaims bounds returned claim triples.
	MaxClaims int
	// MaxOutputTokens bounds the provider response.
	MaxOutputTokens int32
}

// DefaultConfig returns the bounded local-first defaults.
func DefaultConfig() Config {
	return Config{
		Model:             DefaultModel,
		Timeout:           8 * time.Second,
		MaxInputBytes:     8192,
		MaxCandidateBytes: 2048,
		MaxCandidates:     32,
		MaxExpansions:     6,
		MaxClaims:         12,
		MaxOutputTokens:   1024,
	}
}

// ConfigFromEnv reads GEMINI_API_KEY and the optional
// SENTRA_CODE_MEMORY_GEMINI_MODEL override. No key means the adapter stays
// deterministic.
func ConfigFromEnv() Config {
	cfg := DefaultConfig()
	cfg.APIKey = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if m := strings.TrimSpace(os.Getenv("SENTRA_CODE_MEMORY_GEMINI_MODEL")); m != "" {
		cfg.Model = m
	}
	return cfg
}

// Candidate is one bounded scoring input. Text is redacted and truncated
// before transmission; Text may be a snippet, never whole-file source outside
// the bounded candidate set.
type Candidate struct {
	ID   string
	Text string
}

// ClaimTriple is one extracted memory claim (subject-predicate-object plus an
// evidence span), mirroring the memory package's OpenIE contract.
type ClaimTriple struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Span      string `json:"span"`
}

// Diagnostics describe what an operation did without including any prompt,
// source, or credential content.
type Diagnostics struct {
	LLMUsed        bool   `json:"llm_used"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Candidates     int    `json:"candidates,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// Generator is the provider-neutral structured-generation seam. Implementations
// must honor ctx (deadline enforced by Service on top), return raw JSON
// conforming to the operation's contract, and expose no write-capable tools.
type Generator interface {
	GenerateJSON(ctx context.Context, op Operation, system, prompt string, maxOutputTokens int32) (json.RawMessage, error)
	// Describe returns provider and model identifiers for diagnostics.
	Describe() (provider, model string)
}

// Service executes the three bounded operations with deterministic fallback.
type Service struct {
	cfg Config
	gen Generator // nil => deterministic local behavior only
}

// New returns a Service. gen may be nil (deterministic local-only mode).
func New(cfg Config, gen Generator) *Service {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	return &Service{cfg: cfg, gen: gen}
}

var (
	// ErrNoAPIKey reports a Gemini generator requested without GEMINI_API_KEY.
	ErrNoAPIKey = errors.New("llmadapter: GEMINI_API_KEY not configured")

	errContextDone     = errors.New("context_done")
	errDeadlineReserve = errors.New("deadline_reserve")
)

// deadlineReserve is kept between the operation budget and the caller's
// deadline so an optional LLM call never eats the caller's own reserve.
const deadlineReserve = 50 * time.Millisecond

// minBudget is the shortest budget worth a network round-trip.
const minBudget = 100 * time.Millisecond

var (
	absPathRe = regexp.MustCompile(`(\b[A-Za-z]:\\[^\s"']+)|(/(?:[^\s/"'` + "`" + `]+/)+[^\s/"'` + "`" + `]*)`)
	bearerRe  = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
	keyLikeRe = regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{20,}\b`)
)

// redact scrubs credentials, bearer tokens, and absolute paths from s before
// any transmission. It never shortens below the redaction markers' length.
func (c Config) redact(s string) string {
	if c.APIKey != "" && len(c.APIKey) >= 8 {
		s = strings.ReplaceAll(s, c.APIKey, "[redacted-key]")
	}
	s = keyLikeRe.ReplaceAllString(s, "[redacted-key]")
	s = bearerRe.ReplaceAllString(s, "[redacted-token]")
	s = absPathRe.ReplaceAllString(s, "[path]")
	return s
}

// truncate bounds s to n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if n > 0 && len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}

// budget returns the per-operation timeout clamped to the caller's remaining
// deadline minus reserve, or a reason the call must be skipped.
func (c Config) budget(ctx context.Context) (time.Duration, string) {
	if ctx == nil {
		return c.Timeout, ""
	}
	if ctx.Err() != nil {
		return 0, errContextDone.Error()
	}
	d := c.Timeout
	if d <= 0 {
		d = 8 * time.Second
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return d, ""
	}
	avail := time.Until(dl) - deadlineReserve
	if avail < minBudget {
		return 0, errDeadlineReserve.Error()
	}
	if d > avail {
		d = avail
	}
	return d, ""
}

// call runs one bounded, redacted, deadline-clamped generation. ok=false means
// the caller must use the deterministic fallback; diag.FallbackReason says why.
func (s *Service) call(ctx context.Context, op Operation, system, prompt string, diag *Diagnostics) (json.RawMessage, bool) {
	if s.gen == nil {
		diag.FallbackReason = "llm_not_configured"
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	budget, skip := s.cfg.budget(ctx)
	if skip != "" {
		diag.FallbackReason = skip
		return nil, false
	}
	diag.Provider, diag.Model = s.gen.Describe()
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	raw, err := s.gen.GenerateJSON(cctx, op, system, prompt, s.cfg.MaxOutputTokens)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			diag.FallbackReason = errContextDone.Error()
		} else {
			diag.FallbackReason = "provider_error"
		}
		return nil, false
	}
	if len(raw) == 0 {
		diag.FallbackReason = "empty_response"
		return nil, false
	}
	return raw, true
}

// decodeStrict unmarshals raw into v, rejecting unknown fields.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// --- query expansion ---

type expansionResponse struct {
	Queries []string `json:"queries"`
}

// ExpandQuery returns bounded reformulations of query. The deterministic
// fallback returns the normalized original query plus its significant tokens.
func (s *Service) ExpandQuery(ctx context.Context, query string) ([]string, Diagnostics) {
	var diag Diagnostics
	query = s.cfg.redact(truncate(strings.TrimSpace(query), s.cfg.MaxInputBytes))
	if query == "" {
		return nil, diag
	}
	sys := `Expand a code search query into diverse reformulations for retrieval.
Reply with ONLY JSON: {"queries":["...", ...]}.
Keep the original intent. No prose, no commentary.`
	prompt := "max_queries=" + strconv.Itoa(s.cfg.MaxExpansions) + "\nquery=" + query
	raw, ok := s.call(ctx, OpExpandQuery, sys, prompt, &diag)
	if !ok {
		return s.expandFallback(query), diag
	}
	var resp expansionResponse
	if err := decodeStrict(raw, &resp); err != nil {
		diag.FallbackReason = "invalid_response"
		return s.expandFallback(query), diag
	}
	out := normalizeQueries(query, resp.Queries, s.cfg.MaxExpansions)
	if len(out) == 0 {
		diag.FallbackReason = "invalid_response"
		return s.expandFallback(query), diag
	}
	diag.LLMUsed = true
	return out, diag
}

func normalizeQueries(original string, in []string, max int) []string {
	if max <= 0 {
		max = 6
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(q string) {
		q = strings.TrimSpace(q)
		k := strings.ToLower(q)
		if q == "" || len(out) >= max {
			return
		}
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, q)
	}
	add(original) // original intent always leads
	for _, q := range in {
		add(q)
	}
	return out
}

// expandFallback deterministically derives expansions from query tokens.
func (s *Service) expandFallback(query string) []string {
	max := s.cfg.MaxExpansions
	if max <= 0 {
		max = 6
	}
	return dedupeKeepFirst(append([]string{query}, queryTokens(query)...), max)
}

func dedupeKeepFirst(in []string, max int) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, q := range in {
		q = strings.TrimSpace(q)
		k := strings.ToLower(q)
		if q == "" || len(out) >= max {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, q)
	}
	return out
}

// queryTokens splits s into lowercase alphanumeric tokens of length >= 3.
func queryTokens(s string) []string {
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 3 {
			toks = append(toks, cur.String())
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

// --- semantic candidate scoring ---

// Score is one bounded relevance score for a candidate ID.
type Score struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

type scoreResponse struct {
	Scores []Score `json:"scores"`
}

// ScoreCandidates scores a bounded candidate set against query, returning
// scores sorted descending. The deterministic fallback is lexical token
// containment. Unknown candidate IDs in LLM output are dropped; scores are
// clamped to [0,1].
func (s *Service) ScoreCandidates(ctx context.Context, query string, cands []Candidate) ([]Score, Diagnostics) {
	var diag Diagnostics
	query = s.cfg.redact(truncate(strings.TrimSpace(query), s.cfg.MaxInputBytes))
	cands = s.boundCandidates(cands)
	diag.Candidates = len(cands)
	if query == "" || len(cands) == 0 {
		return nil, diag
	}
	sys := `Score each candidate's semantic relevance to the query from 0.0 to 1.0.
Reply with ONLY JSON: {"scores":[{"id":"<candidate id>","score":0.0}, ...]}.
Use only the candidate ids provided. No prose.`
	type promptCand struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	pc := make([]promptCand, 0, len(cands))
	for _, c := range cands {
		pc = append(pc, promptCand{ID: c.ID, Text: c.Text})
	}
	payload, err := json.Marshal(pc)
	if err != nil {
		return lexicalScores(query, cands), diag
	}
	prompt := "query=" + query + "\ncandidates=" + string(payload)
	raw, ok := s.call(ctx, OpScoreCandidates, sys, prompt, &diag)
	if !ok {
		return lexicalScores(query, cands), diag
	}
	var resp scoreResponse
	if err := decodeStrict(raw, &resp); err != nil {
		diag.FallbackReason = "invalid_response"
		return lexicalScores(query, cands), diag
	}
	known := make(map[string]struct{}, len(cands))
	for _, c := range cands {
		known[c.ID] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []Score
	for _, sc := range resp.Scores {
		if _, ok := known[sc.ID]; !ok {
			continue
		}
		if _, dup := seen[sc.ID]; dup {
			continue
		}
		seen[sc.ID] = struct{}{}
		if sc.Score < 0 {
			sc.Score = 0
		}
		if sc.Score > 1 {
			sc.Score = 1
		}
		out = append(out, sc)
	}
	if len(out) == 0 {
		diag.FallbackReason = "invalid_response"
		return lexicalScores(query, cands), diag
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	diag.LLMUsed = true
	return out, diag
}

// boundCandidates redacts, truncates, and caps the candidate set. Only this
// bounded set is ever transmitted.
func (s *Service) boundCandidates(cands []Candidate) []Candidate {
	max := s.cfg.MaxCandidates
	if max <= 0 {
		max = 32
	}
	if len(cands) > max {
		cands = cands[:max]
	}
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			continue
		}
		out = append(out, Candidate{
			ID:   id,
			Text: s.cfg.redact(truncate(c.Text, s.cfg.MaxCandidateBytes)),
		})
	}
	return out
}

// lexicalScores is the deterministic fallback: query-token containment.
func lexicalScores(query string, cands []Candidate) []Score {
	qt := queryTokens(query)
	out := make([]Score, 0, len(cands))
	for _, c := range cands {
		var score float64
		if len(qt) > 0 {
			ct := make(map[string]struct{})
			for _, t := range queryTokens(c.Text) {
				ct[t] = struct{}{}
			}
			hits := 0
			for _, t := range qt {
				if _, ok := ct[t]; ok {
					hits++
				}
			}
			score = float64(hits) / float64(len(qt))
		}
		out = append(out, Score{ID: c.ID, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// --- memory claim extraction ---

type claimsResponse struct {
	Claims []ClaimTriple `json:"claims"`
}

// ExtractClaims returns bounded SPO claim triples for a document. The
// deterministic fallback abstains (nil): deterministic extraction is the
// memory package's job, and an absent LLM must not fabricate claims.
func (s *Service) ExtractClaims(ctx context.Context, docID, text string) ([]ClaimTriple, Diagnostics) {
	var diag Diagnostics
	docID = strings.TrimSpace(docID)
	text = s.cfg.redact(truncate(text, s.cfg.MaxInputBytes))
	if docID == "" || strings.TrimSpace(text) == "" {
		return nil, diag
	}
	sys := `Extract factual subject-predicate-object triples from the document.
Reply with ONLY JSON: {"claims":[{"subject":"...","predicate":"...","object":"...","span":"..."}, ...]}.
High precision only. Abstain with an empty list when unsure. No prose.`
	prompt := "max_claims=" + strconv.Itoa(s.cfg.MaxClaims) + "\ndocument_id=" + docID + "\n\n" + text
	raw, ok := s.call(ctx, OpExtractClaims, sys, prompt, &diag)
	if !ok {
		return nil, diag
	}
	var resp claimsResponse
	if err := decodeStrict(raw, &resp); err != nil {
		diag.FallbackReason = "invalid_response"
		return nil, diag
	}
	max := s.cfg.MaxClaims
	if max <= 0 {
		max = 12
	}
	seen := map[string]struct{}{}
	var out []ClaimTriple
	for _, cl := range resp.Claims {
		sub := strings.TrimSpace(cl.Subject)
		pred := strings.TrimSpace(cl.Predicate)
		obj := strings.TrimSpace(cl.Object)
		if sub == "" || pred == "" || obj == "" {
			continue
		}
		k := strings.ToLower(sub + "|" + pred + "|" + obj)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		span := strings.TrimSpace(cl.Span)
		if span == "" {
			span = sub + " " + pred + " " + obj
		}
		out = append(out, ClaimTriple{Subject: sub, Predicate: pred, Object: obj, Span: span})
		if len(out) >= max {
			break
		}
	}
	diag.LLMUsed = true
	return out, diag
}
