package hosted

import "testing"

func TestWantsMapReduceSynth(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	if !wantsMapReduceSynth("project_related", QueryPlan{}) {
		t.Fatal("project under QUALITY")
	}
	if !wantsMapReduceSynth("completeness", QueryPlan{Completeness: true}) {
		t.Fatal("completeness")
	}
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_BENCH_MAX", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "light")
	t.Setenv("OUROBOROS_ERB_MAP_REDUCE", "0")
	if wantsMapReduceSynth("project_related", QueryPlan{}) {
		t.Fatal("light should skip map-reduce without force")
	}
}
