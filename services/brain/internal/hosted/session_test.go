package hosted

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionTurnGrepRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	t.Setenv("OUROBOROS_BRAIN_SESSION_PATH", path)

	if err := AppendSessionTurn("s1", "user", "We set the RPO target to four hours for payment DB"); err != nil {
		t.Fatal(err)
	}
	if err := AppendSessionTurn("s1", "assistant", "Noted the RPO target."); err != nil {
		t.Fatal(err)
	}
	if err := AppendSessionTurn("s2", "user", "unrelated weather chat"); err != nil {
		t.Fatal(err)
	}

	hist := FormatSessionHistory("s1", 12)
	if hist == "" || !contains(hist, "RPO") {
		t.Fatalf("history=%q", hist)
	}

	hits, diag := TurnGrep("What is the RPO target payment?", "s1", 4, 2)
	if diag["status"] != "ok" {
		t.Fatalf("diag=%v", diag)
	}
	if len(hits) == 0 {
		t.Fatalf("expected turn hits, diag=%v", diag)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	lc, lcd := LongContextFallback("s1", 2000)
	if lcd["status"] != "ok" || lc == "" {
		t.Fatalf("lc=%q diag=%v", lc, lcd)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}

func TestConversationPassagesNeverInAllowedOrPassageIDs(t *testing.T) {
	ps := []Passage{
		{DocumentID: "doc-a", Text: "RPO is 4h", Channel: "lexical"},
		{DocumentID: "turn:s1:1", Text: "we said RPO is 4h", Channel: "turn_grep"},
	}
	if len(passageIDs(ps)) != 1 || passageIDs(ps)[0] != "doc-a" {
		t.Fatalf("passageIDs=%v", passageIDs(ps))
	}
	allow := allowedSet(ps)
	if _, ok := allow["turn:s1:1"]; ok {
		t.Fatal("turn must not be allowed cite")
	}
	g := groundCompletion("RPO is 4h", []string{"turn:s1:1", "doc-a"}, nil, ps, "basic")
	for _, c := range g.CitedDocumentIDs {
		if len(c) >= 5 && c[:5] == "turn:" {
			t.Fatalf("ground leaked turn cite: %v", g.CitedDocumentIDs)
		}
	}
}
