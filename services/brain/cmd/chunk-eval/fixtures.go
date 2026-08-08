// Package main implements chunk-eval: a retrieval-only golden-fixture
// harness that benchmarks chunking strategies (issue #332). It reuses the
// existing product ingestion contract (ChunkStore.UpsertChunks) and lexical
// retrieval (HotLex BM25 projection), and emits diagnostics only — official
// ERB scores are produced exclusively by the pinned ERB harness.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/chunking"
)

// FixtureDocument is one golden corpus document.
type FixtureDocument struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Kind      string `json:"kind"`
	SourceURI string `json:"source_uri"`
}

// FixtureGold names one relevant source span by document plus a unique
// needle token. Needles make gold policy-agnostic: any strategy's chunk that
// contains the needle inside the gold document counts as a hit.
type FixtureGold struct {
	DocumentID string `json:"document_id"`
	Needle     string `json:"needle"`
}

// FixtureQuery is one golden retrieval case.
type FixtureQuery struct {
	QueryID  string        `json:"query_id"`
	Question string        `json:"question"`
	Kind     string        `json:"kind"`
	Gold     []FixtureGold `json:"gold"`
}

// Vocabulary pools for deterministic content generation. Indexing is pure
// arithmetic on document/section/sentence counters, so the corpus is stable
// across runs, machines, and Go versions.
var (
	fillerNouns = []string{
		"ledger", "forecast", "routing", "quota", "beacon", "manifest",
		"schedule", "archive", "compass", "ballast", "signal", "registry",
		"ledgerline", "courier", "lattice", "harbor",
	}
	fillerVerbs = []string{
		"tracks", "bounds", "orders", "splits", "merges", "anchors",
		"guides", "tempers", "shapes", "counts", "seals", "tunes",
	}
	fillerAdjs = []string{
		"steady", "narrow", "golden", "latent", "brisk", "quiet",
		"rugged", "liminal", "candent", "vivid", "mellow", "crisp",
	}
	topicWords = []string{
		"pricing", "onboarding", "failover", "calibration", "telemetry",
		"capacity", "audit", "provisioning", "reconciliation", "migration",
		"escalation", "budgeting",
	}
)

// marker builds the unique probe token for one global section index. The
// trailing two-digit index makes every needle corpus-unique by construction.
func marker(sectionIdx int) string {
	syl1 := []string{"vor", "kel", "thar", "mun", "zeph", "quil", "bran", "solv", "nix", "tar"}
	syl2 := []string{"ath", "une", "irk", "oll", "esh", "iv", "orn", "ux", "al", "em"}
	return syl1[sectionIdx%len(syl1)] + syl2[(sectionIdx*7+3)%len(syl2)] + fmt.Sprintf("%02d", sectionIdx)
}

// fillerSentence builds one deterministic ~17-word sentence.
func fillerSentence(seed int) string {
	adj := fillerAdjs[seed%len(fillerAdjs)]
	n1 := fillerNouns[(seed*3+1)%len(fillerNouns)]
	v := fillerVerbs[(seed*5+2)%len(fillerVerbs)]
	n2 := fillerNouns[(seed*7+3)%len(fillerNouns)]
	n3 := fillerNouns[(seed*11+5)%len(fillerNouns)]
	adj2 := fillerAdjs[(seed*13+7)%len(fillerAdjs)]
	n4 := fillerNouns[(seed*17+11)%len(fillerNouns)]
	return fmt.Sprintf("The %s %s %s the %s while the %s stays within the %s %s margin.",
		adj, n1, v, n2, n3, adj2, n4)
}

// fillerParagraph builds n deterministic sentences seeded off base.
func fillerParagraph(base, n int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fillerSentence(base+i*31))
	}
	return strings.Join(parts, " ")
}

// section is one generated golden region inside a document.
type section struct {
	docID  string
	kind   chunking.Kind
	marker string
	text   string
	topic  []string
}

func topicsFor(seed int) []string {
	return []string{
		topicWords[seed%len(topicWords)],
		topicWords[(seed*5+3)%len(topicWords)],
	}
}

// proseSections builds report-style sections: heading plus long paragraph.
func proseSections(docID string, docIdx int) []section {
	var out []section
	for s := 0; s < 5; s++ {
		g := docIdx*5 + s
		m := marker(g)
		topics := topicsFor(g)
		lead := fmt.Sprintf("Section %d covers %s and %s; the %s probe anchors this region.",
			s+1, topics[0], topics[1], m)
		body := lead + " " + fillerParagraph(g*100, 17)
		out = append(out, section{
			docID:  docID,
			kind:   chunking.KindProse,
			marker: m,
			text:   fmt.Sprintf("## %s overview\n\n%s", topics[0], body),
			topic:  topics,
		})
	}
	return out
}

// codeSections builds modules with a fenced routine whose comment carries the
// marker; fences exercise the code boundary rule.
func codeSections(docID string, docIdx int) []section {
	var out []section
	for s := 0; s < 5; s++ {
		g := 15 + docIdx*5 + s
		m := marker(g)
		topics := topicsFor(g)
		var fence []string
		fence = append(fence, "```python",
			fmt.Sprintf("def routine_%d_%d(payload):", docIdx, s),
			fmt.Sprintf("    # %s calibration path", m))
		for i := 0; i < 38; i++ {
			n := fillerNouns[(g*7+i)%len(fillerNouns)]
			fence = append(fence, fmt.Sprintf("    step_%d = transform(payload, \"%s_%d\")", i, n, i))
		}
		fence = append(fence, "    return summarize(steps)", "```")
		intro := fmt.Sprintf("Module %d owns %s; the %s probe marks its entry point.", s+1, topics[0], m)
		closing := fillerParagraph(g*200, 3)
		out = append(out, section{
			docID:  docID,
			kind:   chunking.KindCode,
			marker: m,
			text:   fmt.Sprintf("## Module %s\n\n%s\n\n%s\n\n%s", topics[1], intro, strings.Join(fence, "\n"), closing),
			topic:  topics,
		})
	}
	return out
}

// tableSections builds a lead sentence (with marker) plus a long markdown
// table per section; rows stay needle-free so gold stays unambiguous.
func tableSections(docID string, docIdx int) []section {
	var out []section
	for s := 0; s < 5; s++ {
		g := 30 + docIdx*5 + s
		m := marker(g)
		topics := topicsFor(g)
		lead := fmt.Sprintf("Table %d records %s figures; %s identifies the sheet.", s+1, topics[0], m)
		rows := []string{"| key | value | note |", "| --- | --- | --- |"}
		for r := 0; r < 30; r++ {
			n := fillerNouns[(g*3+r)%len(fillerNouns)]
			a := fillerAdjs[(g*5+r)%len(fillerAdjs)]
			rows = append(rows, fmt.Sprintf("| %s-%d | %d | %s %s row note |", n, r, (g*31+r)%997, a, n))
		}
		out = append(out, section{
			docID:  docID,
			kind:   chunking.KindTable,
			marker: m,
			text:   fmt.Sprintf("## %s table\n\n%s\n\n%s", topics[1], lead, strings.Join(rows, "\n")),
			topic:  topics,
		})
	}
	return out
}

// slideSections builds one slide per section; sections are later joined with
// separator rules.
func slideSections(docID string, docIdx int) []section {
	var out []section
	for s := 0; s < 5; s++ {
		g := 40 + docIdx*5 + s
		m := marker(g)
		topics := topicsFor(g)
		bullets := []string{fmt.Sprintf("- The %s probe opens this slide on %s.", m, topics[0])}
		for b := 0; b < 16; b++ {
			n := fillerNouns[(g*7+b)%len(fillerNouns)]
			v := fillerVerbs[(g*11+b)%len(fillerVerbs)]
			bullets = append(bullets, fmt.Sprintf("- Slide point %d: the %s %s across the %s lane.", b+1, fillerAdjs[(g+b)%len(fillerAdjs)], v, n))
		}
		out = append(out, section{
			docID:  docID,
			kind:   chunking.KindSlides,
			marker: m,
			text:   fmt.Sprintf("# Slide %d: %s\n\n%s", s+1, topics[1], strings.Join(bullets, "\n")),
			topic:  topics,
		})
	}
	return out
}

// chatSections builds conversation segments; the first turn carries the marker.
func chatSections(docID string, docIdx int) []section {
	speakers := []string{"Alice", "Bob"}
	var out []section
	for s := 0; s < 5; s++ {
		g := 50 + docIdx*5 + s
		m := marker(g)
		topics := topicsFor(g)
		var turns []string
		for turn := 0; turn < 12; turn++ {
			who := speakers[turn%2]
			var line string
			if turn == 0 {
				line = fmt.Sprintf("%s: The %s probe flags %s for this thread.", who, m, topics[0])
			} else {
				line = fmt.Sprintf("%s: %s", who, fillerSentence(g*41+turn*13))
			}
			turns = append(turns, line)
		}
		out = append(out, section{
			docID:  docID,
			kind:   chunking.KindChat,
			marker: m,
			text:   strings.Join(turns, "\n"),
			topic:  topics,
		})
	}
	return out
}

// fixtureDocSpec describes one golden document.
type fixtureDocSpec struct {
	id   string
	kind chunking.Kind
	join string // separator between sections
}

// fixtureDocSpecs is the golden corpus shape: 12 documents, 60 sections.
func fixtureDocSpecs() []fixtureDocSpec {
	return []fixtureDocSpec{
		{id: "doc-prose-1", kind: chunking.KindProse, join: "\n\n"},
		{id: "doc-prose-2", kind: chunking.KindProse, join: "\n\n"},
		{id: "doc-prose-3", kind: chunking.KindProse, join: "\n\n"},
		{id: "doc-code-1", kind: chunking.KindCode, join: "\n\n"},
		{id: "doc-code-2", kind: chunking.KindCode, join: "\n\n"},
		{id: "doc-code-3", kind: chunking.KindCode, join: "\n\n"},
		{id: "doc-table-1", kind: chunking.KindTable, join: "\n\n"},
		{id: "doc-table-2", kind: chunking.KindTable, join: "\n\n"},
		{id: "doc-slides-1", kind: chunking.KindSlides, join: "\n\n---\n\n"},
		{id: "doc-slides-2", kind: chunking.KindSlides, join: "\n\n---\n\n"},
		{id: "doc-chat-1", kind: chunking.KindChat, join: "\n\n"},
		{id: "doc-chat-2", kind: chunking.KindChat, join: "\n\n"},
	}
}

func sectionsFor(spec fixtureDocSpec, docIdx int) []section {
	switch spec.kind {
	case chunking.KindCode:
		return codeSections(spec.id, docIdx)
	case chunking.KindTable:
		return tableSections(spec.id, docIdx)
	case chunking.KindSlides:
		return slideSections(spec.id, docIdx)
	case chunking.KindChat:
		return chatSections(spec.id, docIdx)
	default:
		return proseSections(spec.id, docIdx)
	}
}

// GenerateFixtures builds the deterministic golden corpus and its queries.
// The output is committed under testdata/golden and pinned by
// TestGoldenFixturesAreCurrent.
func GenerateFixtures() ([]FixtureDocument, []FixtureQuery) {
	var docs []FixtureDocument
	var queries []FixtureQuery
	for docIdx, spec := range fixtureDocSpecs() {
		sections := sectionsFor(spec, docIdx)
		texts := make([]string, 0, len(sections))
		for s, sec := range sections {
			texts = append(texts, sec.text)
			q := FixtureQuery{
				QueryID: fmt.Sprintf("q-%s-s%d", spec.id, s+1),
				Kind:    string(spec.kind),
				Gold:    []FixtureGold{{DocumentID: spec.id, Needle: sec.marker}},
			}
			q.Question = fmt.Sprintf("%s %s %s %s overview",
				sec.marker, sec.topic[0], sec.topic[1], spec.kind)
			queries = append(queries, q)
		}
		docs = append(docs, FixtureDocument{
			ID:        spec.id,
			Title:     fmt.Sprintf("Golden %s corpus %d", spec.kind, docIdx+1),
			Body:      strings.Join(texts, spec.join),
			Kind:      string(spec.kind),
			SourceURI: "fixture://" + spec.id,
		})
	}
	return docs, queries
}

// ToSourceDocuments maps fixtures into chunking inputs.
func ToSourceDocuments(docs []FixtureDocument) []chunking.SourceDocument {
	out := make([]chunking.SourceDocument, 0, len(docs))
	for _, d := range docs {
		out = append(out, chunking.SourceDocument{
			ID:        d.ID,
			Title:     d.Title,
			Body:      d.Body,
			SourceURI: d.SourceURI,
			Kind:      chunking.Kind(d.Kind),
		})
	}
	return out
}

// marshalDocuments renders the golden corpus as JSONL bytes.
func marshalDocuments(docs []FixtureDocument) ([]byte, error) {
	var sb strings.Builder
	for _, d := range docs {
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	return []byte(sb.String()), nil
}

// marshalQueries renders the golden query set as JSONL bytes.
func marshalQueries(queries []FixtureQuery) ([]byte, error) {
	var sb strings.Builder
	for _, q := range queries {
		raw, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	return []byte(sb.String()), nil
}
