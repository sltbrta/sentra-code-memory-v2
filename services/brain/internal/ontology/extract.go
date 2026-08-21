package ontology

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// Deterministic extraction patterns (aligned with ERB edges.v1 offline builder).
var (
	dsidRE    = regexp.MustCompile(`(?i)\b(?:dsid_|doc[_-]?)([A-Za-z0-9_./-]{6,})\b`)
	ticketRE  = regexp.MustCompile(`\b([A-Z]{2,10}-\d{2,7})\b`)
	threadRE  = regexp.MustCompile(`(?i)\b(?:thread|conversation|channel)[:\s#]*([A-Za-z0-9_./-]{6,})\b`)
	titleRE   = regexp.MustCompile(`\b([A-Z][a-z]+(?:\s+[A-Z][a-z]+){1,3})\b`)
	allcapsRE = regexp.MustCompile(`\b([A-Z]{3,}(?:[_\s-][A-Z0-9]{2,}){0,3})\b`)
)

const (
	extractBodyCap     = 8_000
	extractTitleCap    = 200
	extractPhraseCap   = 800
	defaultMaxTermDocs = 80
	maxEdgesPerTerm    = 40
	provenanceDet      = "deterministic"
)

// ExtractDocumentEdges finds deterministic document-scoped edges from one body.
//
// Direct citation targets (dsid_/doc_ patterns that differ from docID) emit
// RelCites. Ticket, thread, and co-project signals alone do not yield complete
// doc–doc edges; pair them across a corpus via BuildCoOccurrenceGraph.
func ExtractDocumentEdges(generationID, docID, text string) []Edge {
	if docID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	gen := GenerationID(generationID)
	body := text
	if len(body) > extractBodyCap {
		body = body[:extractBodyCap]
	}
	seen := map[string]struct{}{}
	var edges []Edge
	for _, m := range dsidRE.FindAllStringSubmatch(body, -1) {
		raw := m[0]
		dst := normalizeDocRef(raw)
		if dst == "" || equalFoldDoc(dst, docID) {
			continue
		}
		if _, ok := seen[dst]; ok {
			continue
		}
		seen[dst] = struct{}{}
		edges = append(edges, Edge{
			DocumentSrc:  docID,
			DocumentDst:  dst,
			Rel:          RelCites,
			Weight:       1,
			GenerationID: gen,
			Provenance:   provenanceDet,
		})
	}
	return edges
}

// BuildCoOccurrenceGraph links documents that share rare extracted terms.
// Terms appearing on fewer than 2 or more than maxTermDocs documents are skipped.
// Shared tickets and dsid refs become RelCites; co-project phrases RelCoProject;
// thread keys RelSameThread; other identifier-like tokens RelMentions.
func BuildCoOccurrenceGraph(generationID string, docs map[string]string, maxTermDocs int) Graph {
	gen := GenerationID(generationID)
	g := Graph{GenerationID: gen}
	if len(docs) == 0 {
		return g
	}
	if maxTermDocs <= 0 {
		maxTermDocs = defaultMaxTermDocs
	}

	// (rel, term) → doc ids
	type key struct {
		rel  RelationKind
		term string
	}
	inverted := map[key]map[string]struct{}{}

	for docID, text := range docs {
		if docID == "" {
			continue
		}
		for rel, terms := range termsForDoc(docID, text) {
			for term := range terms {
				if len(term) < 3 {
					continue
				}
				k := key{rel: rel, term: term}
				set, ok := inverted[k]
				if !ok {
					set = map[string]struct{}{}
					inverted[k] = set
				}
				set[docID] = struct{}{}
			}
		}
	}

	// Dedup edges by (src, dst, rel) keeping max weight.
	type ekey struct {
		src, dst string
		rel      RelationKind
	}
	best := map[ekey]float64{}

	for k, set := range inverted {
		n := len(set)
		if n < 2 || n > maxTermDocs {
			continue
		}
		ordered := make([]string, 0, n)
		for id := range set {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		capN := n
		if capN > maxEdgesPerTerm {
			capN = maxEdgesPerTerm
		}
		members := ordered[:capN]
		weight := 1.0 / math.Log(float64(max(2, n)))
		// Star from first (lexicographic hub) + chain for denser multi-hop.
		hub := members[0]
		for _, dst := range members[1:] {
			src, d := orderedPair(hub, dst)
			ek := ekey{src: src, dst: d, rel: k.rel}
			if w, ok := best[ek]; !ok || weight > w {
				best[ek] = weight
			}
		}
		chainW := weight * 0.8
		for i := 0; i < len(members)-1; i++ {
			src, d := orderedPair(members[i], members[i+1])
			ek := ekey{src: src, dst: d, rel: k.rel}
			if w, ok := best[ek]; !ok || chainW > w {
				best[ek] = chainW
			}
		}
	}

	edges := make([]Edge, 0, len(best))
	for ek, w := range best {
		edges = append(edges, Edge{
			DocumentSrc:  ek.src,
			DocumentDst:  ek.dst,
			Rel:          ek.rel,
			Weight:       w,
			GenerationID: gen,
			Provenance:   provenanceDet,
		})
	}
	// Stable order for tests / rebuilds.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].DocumentSrc != edges[j].DocumentSrc {
			return edges[i].DocumentSrc < edges[j].DocumentSrc
		}
		if edges[i].DocumentDst != edges[j].DocumentDst {
			return edges[i].DocumentDst < edges[j].DocumentDst
		}
		return edges[i].Rel < edges[j].Rel
	})
	g.Edges = edges
	return g
}

// termsForDoc extracts typed link terms from one document body.
func termsForDoc(docID, text string) map[RelationKind]map[string]struct{} {
	out := map[RelationKind]map[string]struct{}{
		RelCites:      {},
		RelSameThread: {},
		RelCoProject:  {},
		RelMentions:   {},
	}
	body := text
	if len(body) > extractBodyCap {
		body = body[:extractBodyCap]
	}

	for _, m := range dsidRE.FindAllString(body, -1) {
		out[RelCites][strings.ToLower(m)] = struct{}{}
	}
	for _, m := range ticketRE.FindAllStringSubmatch(body, -1) {
		out[RelCites][strings.ToUpper(m[1])] = struct{}{}
	}
	for _, m := range threadRE.FindAllStringSubmatch(body, -1) {
		out[RelSameThread][strings.ToLower(m[1])] = struct{}{}
	}

	// Co-project: title-like first line + ALLCAPS initiative phrases.
	first := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		first = body[:i]
	}
	if len(first) > extractTitleCap {
		first = first[:extractTitleCap]
	}
	for _, m := range titleRE.FindAllStringSubmatch(first, -1) {
		phrase := strings.TrimSpace(m[1])
		if len(phrase) >= 6 {
			out[RelCoProject][strings.ToLower(phrase)] = struct{}{}
		}
	}
	phraseBody := body
	if len(phraseBody) > extractPhraseCap {
		phraseBody = phraseBody[:extractPhraseCap]
	}
	for _, m := range allcapsRE.FindAllStringSubmatch(phraseBody, -1) {
		phrase := strings.TrimSpace(m[1])
		if len(phrase) >= 3 && len(phrase) <= 40 {
			out[RelCoProject][strings.ToLower(phrase)] = struct{}{}
		}
	}

	// Rare identifier-like tokens (digits mixed, long alnum) as mentions.
	for _, tok := range extractMentionTokens(body) {
		out[RelMentions][tok] = struct{}{}
	}

	// Self-id as cite key so a cite of this doc co-occurs with the owner.
	out[RelCites][strings.ToLower(docID)] = struct{}{}
	return out
}

func extractMentionTokens(body string) []string {
	body = textbound.Bytes(body, 2_000)
	var out []string
	seen := map[string]struct{}{}
	var b strings.Builder
	flush := func() {
		tok := b.String()
		b.Reset()
		if len(tok) < 4 {
			return
		}
		// Require mixed class or a digit to skip common English.
		hasDigit, hasLetter, hasSep := false, false, false
		for _, r := range tok {
			switch {
			case unicode.IsDigit(r):
				hasDigit = true
			case unicode.IsLetter(r):
				hasLetter = true
			case r == '_' || r == '-' || r == '.':
				hasSep = true
			}
		}
		if !hasLetter || (!hasDigit && !hasSep) {
			return
		}
		low := strings.ToLower(tok)
		if _, ok := seen[low]; ok {
			return
		}
		seen[low] = struct{}{}
		out = append(out, low)
	}
	for _, r := range body {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func normalizeDocRef(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Preserve original casing for known dsid_ prefix; otherwise keep as-is.
	return raw
}

func equalFoldDoc(a, b string) bool {
	return strings.EqualFold(a, b)
}

func orderedPair(a, b string) (string, string) {
	if a < b {
		return a, b
	}
	return b, a
}
