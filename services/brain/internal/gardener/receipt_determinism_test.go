package gardener

import (
	"context"
	"fmt"
	"testing"
)

// N-007. hypothesisWorker formatted its receipt Output by ranging over the
// job payload map, so identical input produced a different byte sequence on
// every run. Receipts are digested; a receipt whose bytes are not a function
// of its input defeats the digest and every determinism claim built on it.
// Its neighbour two functions above already sorted, which is what makes this
// an oversight rather than a decision.
//
// Nothing compared two runs of the same job, so reverting the sort left the
// suite green.

func edgeWeightPayload(n int) map[string]string {
	payload := make(map[string]string, n)
	for i := 0; i < n; i++ {
		// A mix either side of the 0.2 threshold, so the output interleaves
		// "downgrade" and "confirm" lines rather than being uniform.
		weight := "0.05"
		if i%2 == 0 {
			weight = "0.90"
		}
		payload[fmt.Sprintf("edge-%03d", i)] = weight
	}
	return payload
}

func TestHypothesisReceiptIsAFunctionOfItsInput(t *testing.T) {
	job := Job{
		ID: "job-1", Kind: JobHypothesisTest, GenerationID: "gen-1",
		DocumentID: "doc-1", Payload: edgeWeightPayload(32),
	}
	first, err := hypothesisWorker{}.Run(context.Background(), job, DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if first.Output == "" {
		t.Fatal("the worker produced no output, so this guard checked nothing")
	}
	for round := 0; round < 16; round++ {
		got, err := hypothesisWorker{}.Run(context.Background(), Job{
			ID: "job-1", Kind: JobHypothesisTest, GenerationID: "gen-1",
			DocumentID: "doc-1", Payload: edgeWeightPayload(32),
		}, DefaultBudget())
		if err != nil {
			t.Fatal(err)
		}
		if got.Output != first.Output {
			t.Fatalf("round %d produced different receipt bytes for identical input:\n"+
				"first:\n%s\ngot:\n%s\nA receipt that is not a function of its input "+
				"cannot be digested.", round, first.Output, got.Output)
		}
	}
}

// TestHypothesisReceiptClassifiesEitherSideOfTheThreshold keeps the guard from
// passing on an empty or uniform output.
func TestHypothesisReceiptClassifiesEitherSideOfTheThreshold(t *testing.T) {
	receipt, err := hypothesisWorker{}.Run(context.Background(), Job{
		ID: "job-2", Kind: JobHypothesisTest, GenerationID: "gen-1",
		Payload: map[string]string{"weak": "0.05", "strong": "0.90"},
	}, DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by key, so "strong" precedes "weak" regardless of the classes
	// they fall into.
	if receipt.Output != "confirm strong\ndowngrade weak\n" {
		t.Fatalf("unexpected receipt body:\n%q", receipt.Output)
	}
}
