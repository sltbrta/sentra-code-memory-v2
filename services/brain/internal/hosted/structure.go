package hosted

import (
	"sort"
	"strings"
	"unicode"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// structureExpander is implemented by MemoryChunkStore and durableStore.
type structureExpander interface {
	StructureExpand(brainID string, seeds []string, maxN int) (edge, entity, facts []string)
	StructureFacts(brainID, question string, limit int) []string
	PassagesForDocs(brainID string, docIDs []string, maxChars int) []Passage
}

// Structure arms (residual parity), single product runtime:
//
//	edge_hop      — co-occur identifier tokens link documents
//	entity_fanout — same token inverted index (entity = edge token)
//	facts_channel — digit/identifier sentences matching the question
//
// Built at index time for product-owned stores; at query time for the SMF
// path2 read path by expanding over the already-retrieved passage pool
// (file/doc-local subgraph reuse — stack-graph framing).

// structureIndex is an inverted name graph over document IDs.
type structureIndex struct {
	// token → document IDs (entities / edge tokens)
	tokenDocs map[string][]string
	// document → tokens
	docTokens map[string][]string
	// document → fact sentences
	docFacts map[string][]string
	// undirected co-occur adjacency: src → dsts
	edges map[string][]string
}

func newStructureIndex() *structureIndex {
	return &structureIndex{
		tokenDocs: map[string][]string{},
		docTokens: map[string][]string{},
		docFacts:  map[string][]string{},
		edges:     map[string][]string{},
	}
}

// indexDocument records entities, facts, and co-occur opportunity for one doc.
// Call finalizeEdges after a batch so co-occur edges are dense enough.
func (s *structureIndex) indexDocument(docID, title, text string) {
	if s == nil || strings.TrimSpace(docID) == "" {
		return
	}
	body := strings.TrimSpace(title + " " + text)
	if body == "" {
		return
	}
	toks := structureEdgeTokens(body)
	s.docTokens[docID] = toks
	for _, t := range toks {
		s.tokenDocs[t] = appendUniqueStr(s.tokenDocs[t], docID)
	}
	s.docFacts[docID] = extractFactSentences(text, 5)
}

// rebuildEdges materializes undirected co-occur edges from tokenDocs.
func (s *structureIndex) rebuildEdges() {
	if s == nil {
		return
	}
	s.edges = map[string][]string{}
	for _, ids := range s.tokenDocs {
		if len(ids) < 2 {
			continue
		}
		if len(ids) > 8 {
			ids = ids[:8]
		}
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				s.edges[ids[i]] = appendUniqueStr(s.edges[ids[i]], ids[j])
				s.edges[ids[j]] = appendUniqueStr(s.edges[ids[j]], ids[i])
			}
		}
	}
}

// edgeExpand returns document IDs co-occurring with seeds (excludes seeds).
func (s *structureIndex) edgeExpand(seeds []string, maxN int) []string {
	if s == nil || maxN <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, id := range seeds {
		seen[id] = struct{}{}
	}
	var out []string
	for _, id := range seeds {
		for _, dst := range s.edges[id] {
			if _, ok := seen[dst]; ok {
				continue
			}
			seen[dst] = struct{}{}
			out = append(out, dst)
			if len(out) >= maxN {
				return out
			}
		}
	}
	return out
}

// entityFanout is alias expansion via shared tokens (same inverted index).
func (s *structureIndex) entityFanout(seeds []string, maxN int) []string {
	if s == nil || maxN <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, id := range seeds {
		seen[id] = struct{}{}
	}
	var out []string
	entitySeen := map[string]struct{}{}
	for _, id := range seeds {
		for _, e := range s.docTokens[id] {
			if _, ok := entitySeen[e]; ok {
				continue
			}
			entitySeen[e] = struct{}{}
			for _, other := range s.tokenDocs[e] {
				if _, ok := seen[other]; ok {
					continue
				}
				seen[other] = struct{}{}
				out = append(out, other)
				if len(out) >= maxN {
					return out
				}
			}
		}
	}
	return out
}

// factsSearch ranks docs whose fact sentences share content tokens with question.
func (s *structureIndex) factsSearch(question string, limit int) []string {
	if s == nil || limit <= 0 {
		return nil
	}
	qtoks := contentTokenSet(question)
	if len(qtoks) == 0 {
		return nil
	}
	type scored struct {
		id string
		sc int
	}
	var arr []scored
	for id, facts := range s.docFacts {
		sc := 0
		for _, f := range facts {
			for t := range contentTokenSet(f) {
				if _, ok := qtoks[t]; ok {
					sc++
				}
			}
		}
		if sc > 0 {
			arr = append(arr, scored{id: id, sc: sc})
		}
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].sc != arr[j].sc {
			return arr[i].sc > arr[j].sc
		}
		return arr[i].id < arr[j].id
	})
	if len(arr) > limit {
		arr = arr[:limit]
	}
	out := make([]string, len(arr))
	for i, a := range arr {
		out[i] = a.id
	}
	return out
}

// structureExpandPassages promotes co-occur / entity / fact neighbors already
// present in the candidate pool (query-time virtual edges). Used on SMF path2
// where we cannot write product structure tables.
func structureExpandPassages(seeds, pool []Passage, maxN int) (neighbors []Passage, diag map[string]any) {
	diag = map[string]any{"structure_mode": "pool_virtual"}
	if maxN <= 0 || len(seeds) == 0 || len(pool) == 0 {
		return nil, diag
	}
	idx := newStructureIndex()
	byID := map[string][]Passage{}
	for _, p := range pool {
		if p.DocumentID == "" {
			continue
		}
		byID[p.DocumentID] = append(byID[p.DocumentID], p)
		// Index once per doc using first passage text as proxy.
		if _, ok := idx.docTokens[p.DocumentID]; !ok {
			idx.indexDocument(p.DocumentID, "", p.Text)
		}
	}
	// Also re-index seeds if missing.
	for _, p := range seeds {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := idx.docTokens[p.DocumentID]; !ok {
			idx.indexDocument(p.DocumentID, "", p.Text)
		}
	}
	idx.rebuildEdges()

	seedIDs := uniqueDocIDs(seeds)
	if len(seedIDs) > 5 {
		seedIDs = seedIDs[:5]
	}
	edgeIDs := idx.edgeExpand(seedIDs, maxN)
	entIDs := idx.entityFanout(seedIDs, maxN)
	// facts over full pool docs
	q := ""
	if len(seeds) > 0 {
		// question not available — use seed text tokens as weak signal; caller
		// should prefer factsSearch with question. Here only edge/entity.
		_ = q
	}
	diag["edge_neighbors"] = edgeIDs
	diag["entity_neighbors"] = entIDs

	seedSet := map[string]struct{}{}
	for _, id := range seedIDs {
		seedSet[id] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []Passage
	for _, id := range append(edgeIDs, entIDs...) {
		if _, ok := seedSet[id]; ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		ps := byID[id]
		if len(ps) == 0 {
			continue
		}
		seen[id] = struct{}{}
		// Promote with modest structure score.
		p := ps[0]
		p.Score = 0.35
		p.Channel = "structure_hop"
		out = append(out, p)
		if len(out) >= maxN {
			break
		}
	}
	diag["structure_promoted"] = len(out)
	return out, diag
}

// factsChannelPassages returns pool passages whose facts match the question.
func factsChannelPassages(question string, pool []Passage, limit int) (hits []Passage, diag map[string]any) {
	diag = map[string]any{}
	if limit <= 0 || len(pool) == 0 {
		return nil, diag
	}
	idx := newStructureIndex()
	byID := map[string]Passage{}
	for _, p := range pool {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := byID[p.DocumentID]; !ok {
			byID[p.DocumentID] = p
			idx.indexDocument(p.DocumentID, "", p.Text)
		}
	}
	ids := idx.factsSearch(question, limit)
	diag["facts_hits"] = ids
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			p.Score = 0.4
			p.Channel = "facts"
			hits = append(hits, p)
		}
	}
	return hits, diag
}

func uniqueDocIDs(ps []Passage) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range ps {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := seen[p.DocumentID]; ok {
			continue
		}
		seen[p.DocumentID] = struct{}{}
		out = append(out, p.DocumentID)
	}
	return out
}

func mergePassagesStructure(base []Passage, extra []Passage, capN int) []Passage {
	if len(extra) == 0 {
		return base
	}
	seen := map[string]struct{}{}
	var out []Passage
	key := func(p Passage) string {
		if p.ChunkID != "" {
			return p.ChunkID
		}
		return p.DocumentID
	}
	for _, p := range base {
		k := key(p)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, p)
	}
	for _, p := range extra {
		k := key(p)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, p)
		if capN > 0 && len(out) >= capN {
			break
		}
	}
	return out
}

// --- token / fact extractors (native reimplementation of productbrain rules) ---

func structureEdgeTokens(text string) []string {
	ids := extractStructureIdentifiers(text)
	// Prefer explicit scan of content tokens for rare identifiers.
	toks := contentTokenSet(text)
	for t := range toks {
		if len(t) >= 5 && (strings.Contains(t, "_") || structureHasDigit(t) || structureTitleish(t)) {
			ids = appendUniqueStr(ids, t)
		}
	}
	if len(ids) > 12 {
		ids = ids[:12]
	}
	return ids
}

func extractStructureIdentifiers(text string) []string {
	// CamelCase, dotted, dashed, underscored tokens with digits or mixed case.
	var out []string
	var b strings.Builder
	flush := func() {
		s := b.String()
		b.Reset()
		if len(s) < 3 {
			return
		}
		// Keep identifier-like only
		if structureHasDigit(s) || strings.ContainsAny(s, "._-") || structureTitleish(s) {
			out = appendUniqueStr(out, strings.ToLower(s))
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func extractFactSentences(text string, maxN int) []string {
	if maxN <= 0 {
		maxN = 5
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})
	var out []string
	seen := map[string]struct{}{}
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if len(s) < 12 {
			continue
		}
		s = textbound.Bytes(s, 280)
		if !structureHasDigit(s) && len(extractStructureIdentifiers(s)) == 0 {
			continue
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
		if len(out) >= maxN {
			break
		}
	}
	return out
}

func structureHasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func structureTitleish(s string) bool {
	if len(s) < 5 {
		return false
	}
	up := 0
	for _, r := range s {
		if unicode.IsUpper(r) {
			up++
		}
	}
	return up >= 2
}

func contentTokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range contentTokens(s) {
		out[t] = struct{}{}
	}
	return out
}

func appendUniqueStr(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
