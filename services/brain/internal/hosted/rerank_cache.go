package hosted

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Issue #301 keeps remote cross-encoder work bounded in two layers:
//
//   - a deterministic lexical/prior-score prefilter selects at most 64
//     candidates (96 is the hard operator ceiling); and
//   - a per-client TTL/LRU caches complete query/passage scores.
//
// The cache is an optimization only. It never stores provider failures or
// incomplete responses, expired entries are removed before use, and an
// incomplete scope identity disables reuse. The original Passage values are
// copied through unchanged except for Score/Channel ranker annotations.
const (
	defaultRerankPrefilterMax = 64
	hardRerankPrefilterMax    = 96
	defaultRerankScoreTTL     = 5 * time.Minute
	defaultRerankScoreMax     = 4096
	hardRerankScoreMax        = 1 << 16
	maxRerankScoreTTL         = time.Hour
	maxRerankCECharactersDiag = 256 * 1024
	maxRerankLatencyDiag      = 2 * time.Minute
	cohereRerankTimeoutCap    = 45 * time.Second
	zeRerankTimeoutCap        = 60 * time.Second
	mlxRerankTimeoutCap       = 60 * time.Second
)

var errRerankDeadlineExhausted = errors.New("rerank request deadline exhausted")

type rerankScoreScope struct {
	ScopeID     string
	Dimension   int
	ACLIdentity string
}

func (s rerankScoreScope) cacheable() bool {
	return s.ScopeID != "" && s.Dimension > 0 && s.ACLIdentity != ""
}

// rerankScope binds cached scores to the authorized brain/generation view,
// embedding dimension, and complete product ACL state. The final key is a
// digest; these raw values never enter diagnostics or stored cache entries.
func (c *Client) rerankScope() rerankScoreScope {
	if c == nil {
		return rerankScoreScope{}
	}
	brainID := strings.TrimSpace(c.cfg.BrainID)
	if brainID == "" {
		return rerankScoreScope{Dimension: c.cfg.CohereDim, ACLIdentity: rerankACLIdentity(c)}
	}
	return rerankScoreScope{
		ScopeID:     digestRerankParts(brainID, c.GenerationID()),
		Dimension:   c.cfg.CohereDim,
		ACLIdentity: rerankACLIdentity(c),
	}
}

func rerankACLIdentity(c *Client) string {
	if c == nil {
		return ""
	}
	parts := []string{
		string(c.Security.Profile),
		strings.TrimSpace(c.Security.Principal),
		strings.TrimSpace(c.Security.Owner),
		strings.TrimSpace(c.Security.BrainID),
	}
	grants := make([]string, 0, len(c.Security.Grants))
	for principal, allowed := range c.Security.Grants {
		grants = append(grants, principal+"="+strconv.FormatBool(allowed))
	}
	sort.Strings(grants)
	parts = append(parts, grants...)
	return digestRerankParts(parts...)
}

func digestRerankParts(parts ...string) string {
	h := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return string(h.Sum(nil))
}

type rerankScoreEntry struct {
	key     string
	score   float64
	expires time.Time
}

type rerankScoreCache struct {
	mu    sync.Mutex
	now   func() time.Time
	ttl   time.Duration
	max   int
	ll    *list.List
	items map[string]*list.Element
}

func newRerankScoreCache(ttl time.Duration, maxEntries int) *rerankScoreCache {
	if ttl <= 0 {
		ttl = defaultRerankScoreTTL
	}
	if ttl > maxRerankScoreTTL {
		ttl = maxRerankScoreTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultRerankScoreMax
	}
	if maxEntries > hardRerankScoreMax {
		maxEntries = hardRerankScoreMax
	}
	return &rerankScoreCache{
		now: time.Now, ttl: ttl, max: maxEntries,
		ll: list.New(), items: make(map[string]*list.Element),
	}
}

func (c *rerankScoreCache) get(key string) (score float64, hit, stale bool) {
	if c == nil {
		return 0, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return 0, false, false
	}
	entry := el.Value.(*rerankScoreEntry)
	if !c.now().Before(entry.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return 0, false, true
	}
	c.ll.MoveToFront(el)
	return entry.score, true, false
}

func (c *rerankScoreCache) put(key string, score float64) {
	if c == nil || key == "" || math.IsNaN(score) || math.IsInf(score, 0) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*rerankScoreEntry)
		entry.score = score
		entry.expires = c.now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&rerankScoreEntry{key: key, score: score, expires: c.now().Add(c.ttl)})
	c.items[key] = el
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		delete(c.items, oldest.Value.(*rerankScoreEntry).key)
		c.ll.Remove(oldest)
	}
}

func (c *rerankScoreCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[string]*list.Element)
}

func rerankPrefilterMax() int {
	n := envInt("OUROBOROS_ERB_RERANK_PREFILTER_N", defaultRerankPrefilterMax)
	if n > hardRerankPrefilterMax {
		return hardRerankPrefilterMax
	}
	return n
}

type rerankPrefilterCandidate struct {
	index    int
	overlap  int
	prior    float64
	identity rerankSourceDigest
}

// rerankPrefilter returns original passage indices. It consults only the user
// question and already-authorized candidates: no question type, expected
// document id, citation, or other evaluator/gold field can enter selection.
func rerankPrefilter(question string, passages []Passage, want int) []int {
	identities := make([]rerankSourceDigest, len(passages))
	for i, passage := range passages {
		identities[i] = rerankSourceIdentity(passage)
	}
	return rerankPrefilterWithIdentities(question, passages, identities, want)
}

func rerankPrefilterWithIdentities(question string, passages []Passage, identities []rerankSourceDigest, want int) []int {
	if len(passages) == 0 || want <= 0 {
		return nil
	}
	if want > len(passages) {
		want = len(passages)
	}
	queryTokens := contentTokens(question)
	candidates := make([]rerankPrefilterCandidate, len(passages))
	for i, passage := range passages {
		lower := strings.ToLower(passage.Text)
		overlap := 0
		for _, token := range queryTokens {
			if strings.Contains(lower, token) {
				overlap++
			}
		}
		prior := passage.Score
		if math.IsNaN(prior) || math.IsInf(prior, 0) {
			prior = 0
		}
		candidates[i] = rerankPrefilterCandidate{
			index: i, overlap: overlap, prior: prior, identity: identities[i],
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].overlap != candidates[j].overlap {
			return candidates[i].overlap > candidates[j].overlap
		}
		if candidates[i].prior != candidates[j].prior {
			return candidates[i].prior > candidates[j].prior
		}
		if order := bytes.Compare(candidates[i].identity[:], candidates[j].identity[:]); order != 0 {
			return order < 0
		}
		return candidates[i].index < candidates[j].index
	})
	out := make([]int, want)
	for i := range out {
		out[i] = candidates[i].index
	}
	return out
}

type rerankSourceDigest [sha256.Size]byte

func rerankSourceIdentity(p Passage) rerankSourceDigest {
	h := sha256.New()
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(p.DocumentID)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(p.DocumentID))
	binary.BigEndian.PutUint64(size[:], uint64(len(p.ChunkID)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(p.ChunkID))
	binary.BigEndian.PutUint64(size[:], uint64(len(p.SourceURI)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(p.SourceURI))
	binary.BigEndian.PutUint64(size[:], uint64(len(p.Text)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(p.Text))
	var digest rerankSourceDigest
	copy(digest[:], h.Sum(nil))
	return digest
}

func rerankQuestionIdentity(question string) rerankSourceDigest {
	// Cross-encoder inputs are exact provider inputs. Case/whitespace changes
	// may change a model score, so unlike the outer retrieve cache this digest
	// deliberately performs no semantic normalization.
	return sha256.Sum256([]byte(question))
}

func rerankScoreKey(backend, model string, questionIdentity rerankSourceDigest, scope rerankScoreScope, sourceIdentity rerankSourceDigest) string {
	h := sha256.New()
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(backend)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(backend))
	binary.BigEndian.PutUint64(size[:], uint64(len(model)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(model))
	binary.BigEndian.PutUint64(size[:], uint64(len(scope.ScopeID)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(scope.ScopeID))
	dimension := strconv.Itoa(scope.Dimension)
	binary.BigEndian.PutUint64(size[:], uint64(len(dimension)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(dimension))
	binary.BigEndian.PutUint64(size[:], uint64(len(scope.ACLIdentity)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(scope.ACLIdentity))
	_, _ = h.Write(sourceIdentity[:])
	_, _ = h.Write(questionIdentity[:])
	return string(h.Sum(nil))
}

type rerankScoreCall func(context.Context, string, []Passage, int) ([]remoteRerankResult, error)

type rerankScoreRun struct {
	input              int
	selected           int
	cacheHits          int
	misses             int
	stales             int
	providerScored     int
	providerSubmitted  bool
	ceCharacters       int
	ceCharactersCapped bool
	providerLatency    time.Duration
	providerTimeout    time.Duration
	failureReason      string
	cacheEnabled       bool
	cacheable          bool
}

// rerankProviderTimeoutCap is the maximum wall time for one provider call.
// The effective call deadline is always the lesser of this cap and the
// caller's remaining request deadline.
func rerankProviderTimeoutCap(backend string) time.Duration {
	switch backend {
	case "cohere":
		return cohereRerankTimeoutCap
	case "zeroentropy":
		return zeRerankTimeoutCap
	case "mlx":
		return mlxRerankTimeoutCap
	default:
		// Unknown remote adapters do not receive a looser envelope.
		return zeRerankTimeoutCap
	}
}

func rerankProviderContext(ctx context.Context, backend string) (context.Context, context.CancelFunc, time.Duration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ctx, func() {}, 0, fmt.Errorf("%w: %v", errRerankDeadlineExhausted, err)
		}
		return ctx, func() {}, 0, err
	}
	capTimeout := rerankProviderTimeoutCap(backend)
	timeout := capTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx, func() {}, 0, errRerankDeadlineExhausted
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return ctx, func() {}, 0, errRerankDeadlineExhausted
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	return callCtx, cancel, timeout, nil
}

func rerankDocumentCharacterLimit(backend string) int {
	if backend == "cohere" {
		return 2000
	}
	return 1500
}

func clippedRerankText(text, backend string) string {
	limit := rerankDocumentCharacterLimit(backend)
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

// rerankCECharacters counts the semantic strings submitted to the provider:
// the query once plus every provider-clipped document. It returns a capped
// diagnostic value so even input lengths cannot become an unbounded side
// channel. The cap flag makes saturation explicit.
func rerankCECharacters(question string, passages []Passage, backend string) (int, bool) {
	total := utf8.RuneCountInString(question)
	if total >= maxRerankCECharactersDiag {
		return maxRerankCECharactersDiag, true
	}
	for _, passage := range passages {
		total += utf8.RuneCountInString(clippedRerankText(passage.Text, backend))
		if total >= maxRerankCECharactersDiag {
			return maxRerankCECharactersDiag, true
		}
	}
	return total, false
}

func rerankFailureReason(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, errRerankDeadlineExhausted):
		return "deadline_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	default:
		return "provider_error"
	}
}

// rerankRemoteBounded applies the issue #301 prefilter/cache contract and
// returns every original candidate exactly once. The promoted head is sorted
// by CE score, then stable source identity, then original index. Unpromoted
// candidates retain their original relative order and citation metadata.
func rerankRemoteBounded(
	ctx context.Context,
	question string,
	passages []Passage,
	topN int,
	backend, model string,
	scope rerankScoreScope,
	cache *rerankScoreCache,
	scoreCall rerankScoreCall,
) ([]Passage, rerankScoreRun, error) {
	run := rerankScoreRun{
		input:        len(passages),
		cacheEnabled: cache != nil,
		cacheable:    cache != nil && scope.cacheable(),
	}
	if len(passages) == 0 {
		return passages, run, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			run.failureReason = "deadline_exhausted"
		} else {
			run.failureReason = rerankFailureReason(err)
		}
		return nil, run, err
	}
	if topN <= 0 || topN > len(passages) {
		topN = len(passages)
	}
	want := topN
	if capN := rerankPrefilterMax(); want > capN {
		want = capN
	}
	identities := make([]rerankSourceDigest, len(passages))
	for i, passage := range passages {
		identities[i] = rerankSourceIdentity(passage)
	}
	selected := rerankPrefilterWithIdentities(question, passages, identities, want)
	run.selected = len(selected)

	scores := make([]float64, len(passages))
	keys := make([]string, len(passages))
	questionIdentity := rerankQuestionIdentity(question)
	missIndices := make([]int, 0, len(selected))
	missPassages := make([]Passage, 0, len(selected))
	for _, index := range selected {
		if run.cacheable {
			key := rerankScoreKey(backend, model, questionIdentity, scope, identities[index])
			keys[index] = key
			if score, hit, stale := cache.get(key); hit {
				scores[index] = score
				run.cacheHits++
				continue
			} else if stale {
				run.stales++
			}
		}
		missIndices = append(missIndices, index)
		missPassages = append(missPassages, passages[index])
	}
	run.misses = len(missIndices)
	if len(missPassages) > 0 {
		if scoreCall == nil {
			run.failureReason = "scorer_unavailable"
			return nil, run, fmt.Errorf("%s rerank scorer unavailable", backend)
		}
		callCtx, cancel, timeout, err := rerankProviderContext(ctx, backend)
		if err != nil {
			run.failureReason = rerankFailureReason(err)
			return nil, run, err
		}
		run.providerTimeout = timeout
		run.providerSubmitted = true
		run.ceCharacters, run.ceCharactersCapped = rerankCECharacters(question, missPassages, backend)
		started := time.Now()
		results, err := scoreCall(callCtx, question, missPassages, len(missPassages))
		run.providerLatency = time.Since(started)
		callContextErr := callCtx.Err()
		cancel()
		if err != nil {
			if callContextErr != nil {
				run.failureReason = rerankFailureReason(callContextErr)
			} else {
				run.failureReason = rerankFailureReason(err)
			}
			return nil, run, err
		}
		validated, err := validateCompleteRerankScores(results, len(missPassages), backend)
		if err != nil {
			run.failureReason = "invalid_response"
			return nil, run, err
		}
		run.providerScored = len(validated)
		for localIndex, score := range validated {
			originalIndex := missIndices[localIndex]
			scores[originalIndex] = score
			if run.cacheable {
				cache.put(keys[originalIndex], score)
			}
		}
	}

	ranked := append([]int(nil), selected...)
	sort.Slice(ranked, func(i, j int) bool {
		left, right := scores[ranked[i]], scores[ranked[j]]
		if left != right {
			return left > right
		}
		leftID := identities[ranked[i]]
		rightID := identities[ranked[j]]
		if order := bytes.Compare(leftID[:], rightID[:]); order != 0 {
			return order < 0
		}
		return ranked[i] < ranked[j]
	})
	promoteN := topN
	if promoteN > len(ranked) {
		promoteN = len(ranked)
	}
	out := make([]Passage, 0, len(passages))
	promoted := make([]bool, len(passages))
	for _, index := range ranked[:promoteN] {
		passage := passages[index]
		passage.Score = scores[index]
		annotateRerankChannel(&passage, backend)
		out = append(out, passage)
		promoted[index] = true
	}
	for index, passage := range passages {
		if !promoted[index] {
			out = append(out, passage)
		}
	}
	if len(out) != len(passages) {
		return nil, run, fmt.Errorf("%s rerank candidate set changed", backend)
	}
	return out, run, nil
}

func annotateRerankChannel(p *Passage, backend string) {
	if p == nil {
		return
	}
	switch backend {
	case "cohere":
		if p.Channel == "" {
			p.Channel = "cohere_rerank"
		} else if !strings.Contains(p.Channel, "cohere") {
			p.Channel += "+cohere"
		}
	case "mlx":
		p.Channel += "+mlx_ce"
	default:
		p.Channel += "+ce"
	}
}

func stampRerankScoreRun(diag map[string]any, run rerankScoreRun) {
	diag["rerank_prefilter_input"] = run.input
	diag["rerank_prefilter_n"] = run.selected
	diag["rerank_prefilter_cap"] = rerankPrefilterMax()
	diag["rerank_cache_hits"] = run.cacheHits
	diag["rerank_cache_misses"] = run.misses
	diag["rerank_cache_stales"] = run.stales
	diag["rerank_provider_scored"] = run.providerScored
	diag["rerank_ce_characters"] = run.ceCharacters
	diag["rerank_ce_characters_capped"] = run.ceCharactersCapped
	diag["rerank_provider_latency_ms"] = boundedRerankDurationMS(run.providerLatency)
	diag["rerank_ce_timeout_ms"] = boundedRerankDurationMS(run.providerTimeout)
	if run.providerSubmitted {
		diag["rerank_ce_cost"] = map[string]any{
			"currency": "USD",
			"status":   "unknown",
			"reason":   "provider_pricing_unavailable",
		}
	} else {
		diag["rerank_ce_cost"] = map[string]any{
			"currency": "USD",
			"status":   "not_incurred",
			"cost_usd": 0.0,
		}
	}
	switch {
	case run.selected == 0:
		diag["rerank_cache"] = "not_applicable"
	case !run.cacheEnabled:
		diag["rerank_cache"] = "disabled"
	case !run.cacheable:
		diag["rerank_cache"] = "identity_incomplete"
	case run.misses == 0:
		diag["rerank_cache"] = "hit"
	case run.cacheHits > 0:
		diag["rerank_cache"] = "partial"
	default:
		diag["rerank_cache"] = "miss"
	}
}

func boundedRerankDurationMS(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	if duration > maxRerankLatencyDiag {
		return maxRerankLatencyDiag.Milliseconds()
	}
	return duration.Milliseconds()
}
