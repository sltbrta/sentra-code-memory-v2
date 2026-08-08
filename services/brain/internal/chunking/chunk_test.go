package chunking

import (
	"strings"
	"testing"
)

func TestTokenizeOffsetsRoundTrip(t *testing.T) {
	src := "  alpha beta\tgamma\n\ndelta "
	toks := Tokenize(src)
	want := []string{"alpha", "beta", "gamma", "delta"}
	if len(toks) != len(want) {
		t.Fatalf("tokens = %d, want %d", len(toks), len(want))
	}
	for i, tok := range toks {
		if tok.Text != want[i] {
			t.Errorf("token %d = %q, want %q", i, tok.Text, want[i])
		}
		if src[tok.Start:tok.End] != tok.Text {
			t.Errorf("token %d offsets do not round-trip: %q", i, src[tok.Start:tok.End])
		}
	}
	if CountTokens(src) != len(want) {
		t.Errorf("CountTokens = %d, want %d", CountTokens(src), len(want))
	}
	if got := Tokenize("   "); len(got) != 0 {
		t.Errorf("whitespace-only should tokenize empty, got %v", got)
	}
}

func TestPolicyValidateAndBaseline(t *testing.T) {
	base := BaselinePolicy()
	if err := base.Validate(); err != nil {
		t.Fatalf("baseline policy invalid: %v", err)
	}
	if base.TargetTokens != 500 || base.OverlapTokens < 50 {
		t.Fatalf("baseline must be 500-token target with >= 50 overlap, got %d/%d",
			base.TargetTokens, base.OverlapTokens)
	}
	if !base.MeetsBaseline() {
		t.Error("baseline policy must satisfy MeetsBaseline")
	}
	for _, p := range EvalStrategies() {
		if err := p.Validate(); err != nil {
			t.Fatalf("eval strategy %s invalid: %v", p.Strategy, err)
		}
	}

	bad := []Policy{
		{}, // empty
		{ID: "x", Version: 0, TokenizerID: TokenizerID, Strategy: StrategyFixed, TargetTokens: 500, OverlapTokens: 50},
		{ID: "x", Version: 1, Strategy: StrategyFixed, TargetTokens: 500, OverlapTokens: 50},
		{ID: "x", Version: 1, TokenizerID: TokenizerID, Strategy: "sideways", TargetTokens: 500, OverlapTokens: 50},
		{ID: "x", Version: 1, TokenizerID: TokenizerID, Strategy: StrategyFixed, TargetTokens: 4, OverlapTokens: 1},
		{ID: "x", Version: 1, TokenizerID: TokenizerID, Strategy: StrategyFixed, TargetTokens: 100, OverlapTokens: 100},
		{ID: "x", Version: 1, TokenizerID: TokenizerID, Strategy: StrategyFixed, TargetTokens: 100, OverlapTokens: -1},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("bad policy %d passed validation: %+v", i, p)
		}
	}

	pc := ParentChildPolicy()
	if pc.ChildTargetTokens >= pc.ParentTargetTokens {
		t.Error("parent-child policy must size children below parents")
	}
	swapped := pc
	swapped.ChildTargetTokens = swapped.ParentTargetTokens + 1
	if err := swapped.Validate(); err == nil {
		t.Error("child >= parent target must fail validation")
	}
}

// words builds deterministic filler text of exactly n whitespace tokens.
func words(prefix string, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(prefix)
		sb.WriteByte('a' + byte(i%26)) //nolint:gosec // deterministic filler
		sb.WriteByte('a' + byte((i/26)%26))
	}
	return sb.String()
}

func docOf(id, body string, kind Kind) SourceDocument {
	return SourceDocument{ID: id, Title: "Title of " + id, Body: body, Kind: kind}
}

func TestFixedChunkHonorsTargetAndOverlap(t *testing.T) {
	p := BaselinePolicy()
	src := docOf("d1", words("w", 1200), KindProse)
	receipts, err := Chunk([]SourceDocument{src}, p)
	if err != nil {
		t.Fatal(err)
	}
	full := src.Source()
	if len(receipts) < 3 {
		t.Fatalf("1200 tokens at target 500 should produce >= 3 chunks, got %d", len(receipts))
	}
	prevEnd := -1
	for _, r := range receipts {
		if r.Tokens > p.TargetTokens {
			t.Errorf("chunk %s has %d tokens > target %d", r.ChunkID, r.Tokens, p.TargetTokens)
		}
		if full[r.Start:r.End] != r.Text {
			t.Errorf("chunk %s offsets do not slice its text", r.ChunkID)
		}
		if r.Start <= prevEnd && prevEnd >= 0 {
			// Overlap is allowed, but every chunk must strictly advance.
			if r.End <= prevEnd {
				t.Errorf("chunk %s does not advance past previous end", r.ChunkID)
			}
		}
		prevEnd = r.End
	}
	// Overlap: consecutive windows must share at least 50 tokens.
	a, b := Tokenize(receipts[0].Text), Tokenize(receipts[1].Text)
	shared := 0
	seen := map[string]int{}
	for _, tok := range a {
		seen[tok.Text]++
	}
	for _, tok := range b {
		if seen[tok.Text] > 0 {
			shared++
			seen[tok.Text]--
		}
	}
	if shared < BaselineOverlapTokens {
		t.Errorf("consecutive chunks share %d tokens, want >= %d", shared, BaselineOverlapTokens)
	}
}

func TestRebuildIdentityIsDeterministic(t *testing.T) {
	docs := []SourceDocument{
		docOf("d1", words("alpha", 700), KindProse),
		docOf("d2", words("beta", 300), KindCode),
	}
	policies := EvalStrategies()
	for _, p := range policies {
		first, err := Chunk(docs, p)
		if err != nil {
			t.Fatalf("%s: %v", p.Strategy, err)
		}
		second, err := Chunk(docs, p)
		if err != nil {
			t.Fatalf("%s rebuild: %v", p.Strategy, err)
		}
		if len(first) != len(second) {
			t.Fatalf("%s: rebuild changed receipt count %d -> %d", p.Strategy, len(first), len(second))
		}
		for i := range first {
			if first[i] != second[i] {
				t.Fatalf("%s: rebuild identity broken at receipt %d:\n%+v\n%+v",
					p.Strategy, i, first[i], second[i])
			}
		}
	}
}

func TestReceiptsCarryContractStamps(t *testing.T) {
	p := BaselinePolicy()
	docs := []SourceDocument{docOf("d1", words("s", 80), KindProse)}
	receipts, err := Chunk(docs, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("small doc should be one chunk, got %d", len(receipts))
	}
	r := receipts[0]
	if r.PolicyID != PolicyID || r.PolicyVersion != BaselineVersion || r.TokenizerID != TokenizerID {
		t.Errorf("missing policy/tokenizer stamps: %+v", r)
	}
	if r.Strategy != string(p.Strategy) || r.Kind != string(KindProse) {
		t.Errorf("missing strategy/kind stamps: %+v", r)
	}
	if r.SHA256 == "" || len(r.SHA256) != 64 {
		t.Errorf("sha256 stamp malformed: %q", r.SHA256)
	}
	if !strings.Contains(r.ChunkID, "d1#") {
		t.Errorf("chunk id should be document-scoped: %q", r.ChunkID)
	}
}

func TestStructureAwareBoundaries(t *testing.T) {
	// Build a code doc whose fenced block straddles the 500-token boundary:
	// any split inside the fence would break fence parity.
	prose := words("intro", 450)
	fence := "```python\n" + words("line", 200) + "\n```"
	tail := words("outro", 100)
	doc := docOf("code1", prose+"\n\n"+fence+"\n\n"+tail, KindCode)

	receipts, err := Chunk([]SourceDocument{doc}, StructurePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) < 2 {
		t.Fatalf("expected multiple structure chunks, got %d", len(receipts))
	}
	full := doc.Source()
	for _, r := range receipts {
		opened := strings.Count(r.Text, "```")
		if opened%2 != 0 {
			t.Errorf("chunk %s splits inside a fenced block (odd fence count %d)", r.ChunkID, opened)
		}
		if full[r.Start:r.End] != r.Text {
			t.Errorf("chunk %s offsets do not slice its text", r.ChunkID)
		}
	}
}

func TestStructureTableStaysWhole(t *testing.T) {
	var rows []string
	for i := 0; i < 80; i++ {
		rows = append(rows, "| row"+words("cell", 3)+" | "+words("val", 2)+" |")
	}
	table := "| a | b |\n| - | - |\n" + strings.Join(rows, "\n")
	doc := docOf("t1", words("lead", 50)+"\n\n"+table, KindTable)
	receipts, err := Chunk([]SourceDocument{doc}, StructurePolicy())
	if err != nil {
		t.Fatal(err)
	}
	// The table exceeds 500 tokens, so it must split at row boundaries: every
	// chunk that holds table rows holds only complete pipe-delimited lines.
	for _, r := range receipts {
		for _, line := range strings.Split(r.Text, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if strings.Contains(trimmed, "|") && !strings.HasPrefix(trimmed, "|") {
				t.Errorf("chunk %s splits a table row mid-line: %q", r.ChunkID, line)
			}
		}
	}
}

func TestStructureSlidesAndChatBoundaries(t *testing.T) {
	slides := docOf("s1",
		words("slide1", 60)+"\n\n---\n\n"+words("slide2", 60)+"\n\n---\n\n"+words("slide3", 60),
		KindSlides)
	p := StructurePolicy()
	p.TargetTokens = 90 // force one slide per chunk
	receipts, err := Chunk([]SourceDocument{slides}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 3 {
		t.Fatalf("three slides should produce three chunks at target 90, got %d", len(receipts))
	}
	for _, r := range receipts {
		trimmed := strings.TrimSpace(r.Text)
		if strings.HasPrefix(trimmed, "---") || strings.HasSuffix(trimmed, "---") {
			t.Errorf("slide chunk %s starts or ends with a separator", r.ChunkID)
		}
	}

	chat := docOf("c1",
		"Alice: "+words("ask", 40)+"\nBob: "+words("reply", 40)+"\nAlice: "+words("followup", 40),
		KindChat)
	p2 := StructurePolicy()
	p2.TargetTokens = 60 // one turn per chunk
	chatReceipts, err := Chunk([]SourceDocument{chat}, p2)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReceipts) != 3 {
		t.Fatalf("three turns should produce three chunks at target 60, got %d", len(chatReceipts))
	}
	for _, r := range chatReceipts {
		speakers := 0
		for _, prefix := range []string{"Alice:", "Bob:"} {
			if strings.Contains(r.Text, prefix) {
				speakers++
			}
		}
		if speakers != 1 {
			t.Errorf("chat chunk %s mixes speakers: %q", r.ChunkID, r.Text)
		}
	}
}

func TestParentChildLinksAndContainment(t *testing.T) {
	p := ParentChildPolicy()
	doc := docOf("d1", words("pc", 2500), KindProse)
	receipts, err := Chunk([]SourceDocument{doc}, p)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Receipt{}
	var parents, children []Receipt
	for _, r := range receipts {
		byID[r.ChunkID] = r
		if r.Role == RoleParent {
			parents = append(parents, r)
		} else {
			children = append(children, r)
		}
	}
	if len(parents) == 0 || len(children) == 0 {
		t.Fatalf("parent-child must emit both roles: %d parents, %d children", len(parents), len(children))
	}
	for _, c := range children {
		if c.ParentID == "" {
			t.Fatalf("child %s has no parent id", c.ChunkID)
		}
		parent, ok := byID[c.ParentID]
		if !ok {
			t.Fatalf("child %s parent %s does not resolve", c.ChunkID, c.ParentID)
		}
		if parent.DocumentID != c.DocumentID {
			t.Errorf("child %s parent crosses documents", c.ChunkID)
		}
		if c.Start < parent.Start || c.End > parent.End {
			t.Errorf("child %s [%d,%d) escapes parent [%d,%d)",
				c.ChunkID, c.Start, c.End, parent.Start, parent.End)
		}
	}
}

func TestWholeDocMatchesNaiveShape(t *testing.T) {
	doc := docOf("d1", words("nv", 90), KindProse)
	receipts, err := Chunk([]SourceDocument{doc}, NaivePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("whole_doc must emit one chunk per document, got %d", len(receipts))
	}
	if receipts[0].Text != doc.Source() {
		t.Error("whole_doc chunk must equal the full source string")
	}
}

func TestChunkSkipsEmptyAndUnidentifiedDocs(t *testing.T) {
	docs := []SourceDocument{
		{ID: "", Body: "text without id"},
		{ID: "blank", Body: "   "},
	}
	for _, p := range EvalStrategies() {
		receipts, err := Chunk(docs, p)
		if err != nil {
			t.Fatalf("%s: %v", p.Strategy, err)
		}
		if len(receipts) != 0 {
			t.Errorf("%s should skip empty/unidentified docs, got %d receipts", p.Strategy, len(receipts))
		}
	}
}
