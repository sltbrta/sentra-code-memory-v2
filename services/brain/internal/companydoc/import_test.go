package companydoc

import (
	"strings"
	"testing"
)

func TestImportJSONLAndValidate(t *testing.T) {
	t.Parallel()
	raw := `{"document_id":"d1","text":"MedThink RPO is 15 minutes.","source_types":["confluence"]}
{"document_id":"d2","text":"Follow-up PROJ-99 mitigation.","title":"ticket"}
`
	b, err := ImportJSONL(strings.NewReader(raw), "src-co", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Documents) != 2 {
		t.Fatalf("docs=%d", len(b.Documents))
	}
	if b.Documents[0].Digest() == "" {
		t.Fatal("empty digest")
	}
	m := TextMap(b.Documents)
	if m["d1"] == "" || m["d2"] == "" {
		t.Fatalf("textmap=%v", m)
	}
}

func TestValidateRejectsDup(t *testing.T) {
	t.Parallel()
	err := ValidateBatch(Batch{
		SourceID: "s", GenerationID: "g",
		Documents: []Document{{ID: "a", Text: "x"}, {ID: "a", Text: "y"}},
	})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}
