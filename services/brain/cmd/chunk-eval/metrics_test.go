package main

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHitAtK(t *testing.T) {
	if !hitAtK([]int{3}, 5) {
		t.Error("rank 3 within k=5 must hit")
	}
	if hitAtK([]int{6}, 5) {
		t.Error("rank 6 outside k=5 must miss")
	}
	if hitAtK(nil, 5) {
		t.Error("no gold ranks must miss")
	}
}

func TestMeanReciprocalRank(t *testing.T) {
	// First gold ranks: 1, 2, miss, 4 -> (1 + 1/2 + 0 + 1/4)/4 = 0.4375.
	got := meanReciprocalRank([]int{1, 2, 0, 4})
	if !almostEqual(got, 0.4375) {
		t.Errorf("MRR = %v, want 0.4375", got)
	}
	if meanReciprocalRank(nil) != 0 {
		t.Error("empty MRR must be 0")
	}
}

func TestNDCGAtK(t *testing.T) {
	// Perfect ranking with one gold: NDCG = 1.
	if got := ndcgAtK([]int{1, 0, 0}, 1, 3); !almostEqual(got, 1) {
		t.Errorf("perfect NDCG = %v, want 1", got)
	}
	// One gold at rank 2 with k=2: DCG = 1/log2(3), ideal = 1/log2(2) = 1.
	want := (1 / math.Log2(3)) / 1
	if got := ndcgAtK([]int{0, 1}, 1, 2); !almostEqual(got, want) {
		t.Errorf("NDCG = %v, want %v", got, want)
	}
	// Two golds within k=2 is still perfect.
	if got := ndcgAtK([]int{1, 1}, 2, 2); !almostEqual(got, 1) {
		t.Errorf("two-gold NDCG = %v, want 1", got)
	}
	if ndcgAtK([]int{0, 0}, 0, 2) != 0 {
		t.Error("zero gold NDCG must be 0")
	}
}

func TestPercentile(t *testing.T) {
	values := []float64{9, 1, 5, 3, 7}
	if got := percentile(values, 50); got != 5 {
		t.Errorf("p50 = %v, want 5", got)
	}
	if got := percentile(values, 100); got != 9 {
		t.Errorf("p100 = %v, want 9", got)
	}
	if percentile(nil, 50) != 0 {
		t.Error("empty percentile must be 0")
	}
}
