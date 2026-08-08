package hosted

import "testing"

func TestForceInfoNotFoundAlwaysCaveats(t *testing.T) {
	// Invented rate must not pass as a clean answer.
	out := forceInfoNotFoundAbstention(
		"The default burst surcharge is $0.40 per 1k tokens. The documents do not state the GL account.",
	)
	if out == "" || out[:20] != "The query is not ful" {
		t.Fatalf("must lead with caveat, got %q", out[:min(80, len(out))])
	}
	// Pure caveat preserved.
	caveat := "The query is not fully answerable from the supplied documents; the documents do not establish the requested specifics."
	if forceInfoNotFoundAbstention(caveat) != caveat {
		t.Fatal("idempotent on full caveat")
	}
}

func TestInventsNumericDetail(t *testing.T) {
	if !inventsNumericDetail("$0.40 per 1k tokens") {
		t.Fatal("expected invent detect")
	}
	if inventsNumericDetail("no numbers here about policy") {
		t.Fatal("false positive")
	}
}
