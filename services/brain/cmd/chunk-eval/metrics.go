package main

import (
	"math"
	"sort"
)

// hitAtK reports whether any of the top-k ranks (0-based hit positions) is a
// gold hit. ranks holds 1-based ranks of gold hits for one query.
func hitAtK(goldRanks []int, k int) bool {
	for _, r := range goldRanks {
		if r >= 1 && r <= k {
			return true
		}
	}
	return false
}

// meanReciprocalRank averages 1/firstGoldRank over queries; queries without a
// gold hit contribute 0.
func meanReciprocalRank(firstRanks []int) float64 {
	if len(firstRanks) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range firstRanks {
		if r >= 1 {
			sum += 1.0 / float64(r)
		}
	}
	return sum / float64(len(firstRanks))
}

// ndcgAtK computes NDCG with binary relevance: rel[i] is 1 when rank i+1 is
// a gold hit. The ideal places all golds at the head.
func ndcgAtK(rel []int, goldCount, k int) float64 {
	if len(rel) > k {
		rel = rel[:k]
	}
	dcg := 0.0
	for i, r := range rel {
		if r > 0 {
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}
	ideal := 0.0
	g := goldCount
	if g > k {
		g = k
	}
	for i := 0; i < g; i++ {
		ideal += 1.0 / math.Log2(float64(i+2))
	}
	if ideal == 0 {
		return 0
	}
	return dcg / ideal
}

// percentile returns the p-th percentile (0-100) of a sorted copy of values.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(p/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
