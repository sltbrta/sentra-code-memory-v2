package companydoc

import (
	"context"
	"strings"
	"testing"
)

func TestLiveCorpusRetrieveAndAnswer(t *testing.T) {
	t.Parallel()
	raw := `{"document_id":"d1","text":"MedThink RPO is 15 minutes for active datasets. PROJ-99."}
{"document_id":"d2","text":"PROJ-99 follow-up mitigation for MedThink failover."}
{"document_id":"d3","text":"Picnic weather forecast unrelated."}
`
	batch, err := ImportJSONL(strings.NewReader(raw), "src", "gen-live")
	if err != nil {
		t.Fatal(err)
	}
	corp, err := OpenLive(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	ids, diag := corp.Retrieve(context.Background(), "What is MedThink RPO?", 4)
	if len(ids) == 0 {
		t.Fatalf("empty retrieve diag=%v", diag)
	}
	if ids[0] != "d1" && ids[0] != "d2" {
		t.Fatalf("top=%v want d1/d2", ids)
	}
	ans, cited, _ := corp.Answer(context.Background(), "What is MedThink RPO?", 4)
	if ans == "" || len(cited) == 0 {
		t.Fatalf("answer empty cited=%v", cited)
	}
	if !strings.Contains(ans, "MedThink") && !strings.Contains(ans, "RPO") {
		t.Fatalf("answer missing evidence: %s", ans)
	}
}
