package gardener

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Lifecycle job kinds (Phase 3 Lattice-class consolidation on product gardener).
const (
	JobPredictCalibrate JobKind = "predict_calibrate"
	JobNREMConsolidate  JobKind = "nrem_consolidate"
	JobREMReencode      JobKind = "rem_reencode"
	JobUtilityDecay     JobKind = "utility_decay"
	JobHypothesisTest   JobKind = "hypothesis_test"
	JobEpisodeReseg     JobKind = "episode_reseg"
	JobWeakEdgePrune    JobKind = "weak_edge_prune"
	JobGCQuarantine     JobKind = "gc_quarantine"
)

// LifecyclePolicy orders consolidation phases (GDN lifecycle order).
// Inspiration: Lattice GARDENER, SFS sfs-gardener, Gardener_Agent_v1 (concepts only).
type LifecyclePolicy struct {
	// EnableREM allows costly REM re-encode (default off — polish/latency).
	EnableREM bool
	// PredictionError is measured probe error; when < Threshold, skip heavy phases.
	// Prefer MeasurePredictionError from memory.C1 probes rather than a constant.
	PredictionError float64
	// Threshold default 0.15 when zero.
	Threshold float64
	// Utility scores keyed by document id (E4) — seed for decay worker payload.
	Utility map[string]float64
	// Edges is a simple weighted edge list "a->b" → weight for C5/prune.
	Edges map[string]float64
	// Now overrides clock for deterministic tests.
	Now func() time.Time
	// OnUtilityDecay is invoked with decayed scores so product memory can
	// close the ranking loop (utility → retrieve). Optional.
	OnUtilityDecay func(scores map[string]float64)
	// OnRAPTOR is invoked with document texts for hierarchical summary jobs.
	OnRAPTOR func(documents map[string]string)
}

// ShouldSkipHeavy is C1 predict-calibrate gate (GDN-004).
func (p LifecyclePolicy) ShouldSkipHeavy() bool {
	th := p.Threshold
	if th <= 0 {
		th = 0.15
	}
	return p.PredictionError >= 0 && p.PredictionError < th
}

// PlanLifecycleJobs builds ordered lifecycle jobs for a generation (may be empty if C1 skips).
func PlanLifecycleJobs(generationID string, documents map[string]string, pol LifecyclePolicy) []Job {
	if generationID == "" {
		return nil
	}
	now := time.Now
	if pol.Now != nil {
		now = pol.Now
	}
	t0 := now()
	// Always emit predict_calibrate receipt job first.
	jobs := []Job{{
		ID:   stableJobID(generationID, JobPredictCalibrate, "_gate"),
		Kind: JobPredictCalibrate, GenerationID: generationID, DocumentID: "_gate",
		Payload: map[string]string{
			"prediction_error": fmt.Sprintf("%g", pol.PredictionError),
			"threshold":        fmt.Sprintf("%g", pol.Threshold),
			"skip_heavy":       strconv.FormatBool(pol.ShouldSkipHeavy()),
		},
		CreatedAt: t0,
	}}
	if pol.ShouldSkipHeavy() {
		return jobs
	}
	// Utility decay once per wave.
	jobs = append(jobs, Job{
		ID:   stableJobID(generationID, JobUtilityDecay, "_all"),
		Kind: JobUtilityDecay, GenerationID: generationID, DocumentID: "_all",
		Payload: utilityPayload(pol.Utility), CreatedAt: t0,
	})
	// NREM per document (deterministic consolidate marker).
	ids := sortedDocIDs(documents)
	for _, id := range ids {
		jobs = append(jobs, Job{
			ID:   stableJobID(generationID, JobNREMConsolidate, id),
			Kind: JobNREMConsolidate, GenerationID: generationID, DocumentID: id,
			Payload:   map[string]string{"text": truncatePayload(documents[id], payloadTextCap)},
			CreatedAt: t0,
		})
	}
	if pol.EnableREM {
		for _, id := range ids {
			jobs = append(jobs, Job{
				ID:   stableJobID(generationID, JobREMReencode, id),
				Kind: JobREMReencode, GenerationID: generationID, DocumentID: id,
				Payload:   map[string]string{"text": truncatePayload(documents[id], 800)},
				CreatedAt: t0,
			})
		}
	}
	jobs = append(jobs,
		Job{
			ID:   stableJobID(generationID, JobHypothesisTest, "_edges"),
			Kind: JobHypothesisTest, GenerationID: generationID, DocumentID: "_edges",
			Payload: edgePayload(pol.Edges), CreatedAt: t0,
		},
		Job{
			ID:   stableJobID(generationID, JobWeakEdgePrune, "_edges"),
			Kind: JobWeakEdgePrune, GenerationID: generationID, DocumentID: "_edges",
			Payload: edgePayload(pol.Edges), CreatedAt: t0,
		},
		Job{
			ID:   stableJobID(generationID, JobGCQuarantine, "_all"),
			Kind: JobGCQuarantine, GenerationID: generationID, DocumentID: "_all",
			Payload: map[string]string{"docs": strconv.Itoa(len(ids))}, CreatedAt: t0,
		},
		Job{
			ID:   stableJobID(generationID, JobEpisodeReseg, "_conv"),
			Kind: JobEpisodeReseg, GenerationID: generationID, DocumentID: "_conv",
			Payload: map[string]string{"enabled": "1"}, CreatedAt: t0,
		},
	)
	return jobs
}

// LifecycleWorkers returns deterministic workers for lifecycle kinds.
func LifecycleWorkers() map[JobKind]Worker {
	return map[JobKind]Worker{
		JobPredictCalibrate: predictCalibrateWorker{},
		JobNREMConsolidate:  nremWorker{},
		JobREMReencode:      remWorker{},
		JobUtilityDecay:     utilityDecayWorker{},
		JobHypothesisTest:   hypothesisWorker{},
		JobEpisodeReseg:     episodeWorker{},
		JobWeakEdgePrune:    pruneWorker{},
		JobGCQuarantine:     gcWorker{},
	}
}

// MergeWorkers combines base and extra worker maps (extra wins).
func MergeWorkers(base, extra map[JobKind]Worker) map[JobKind]Worker {
	out := make(map[JobKind]Worker, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// RunLifecycle enqueues lifecycle jobs and runs one scheduler wave.
// Does not touch primary evidence bytes — only queue receipts (GDN-002).
// When heavy work runs, OnUtilityDecay/OnRAPTOR close the product memory loop.
func RunLifecycle(ctx context.Context, q Queue, generationID string, documents map[string]string, pol LifecyclePolicy, budget Budget) ([]Receipt, error) {
	if q == nil {
		return nil, fmt.Errorf("gardener: nil queue")
	}
	jobs := PlanLifecycleJobs(generationID, documents, pol)
	if len(jobs) == 0 {
		return nil, nil
	}
	if err := q.Enqueue(ctx, jobs...); err != nil {
		return nil, err
	}
	workers := MergeWorkers(DefaultWorkers(), LifecycleWorkers())
	// Wrap utility decay to call product hook after worker output.
	if pol.OnUtilityDecay != nil {
		base := workers[JobUtilityDecay]
		workers[JobUtilityDecay] = utilityHookWorker{inner: base, hook: pol.OnUtilityDecay, seed: pol.Utility}
	}
	s := Scheduler{Queue: q, Workers: workers, Budget: budget}
	recs, err := s.RunWave(ctx, "lifecycle")
	if err != nil {
		return recs, err
	}
	if !pol.ShouldSkipHeavy() && pol.OnRAPTOR != nil {
		pol.OnRAPTOR(documents)
	}
	return recs, nil
}

type utilityHookWorker struct {
	inner Worker
	hook  func(map[string]float64)
	seed  map[string]float64
}

func (w utilityHookWorker) Kind() JobKind {
	if w.inner != nil {
		return w.inner.Kind()
	}
	return JobUtilityDecay
}

func (w utilityHookWorker) Run(ctx context.Context, job Job, budget Budget) (Receipt, error) {
	var rec Receipt
	var err error
	if w.inner != nil {
		rec, err = w.inner.Run(ctx, job, budget)
	} else {
		rec = Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true}
	}
	// Apply decay to seed map and publish.
	out := map[string]float64{}
	for k, v := range w.seed {
		out[k] = v * 0.95
		if out[k] < 0.01 {
			out[k] = 0.01
		}
	}
	// Also parse job payload scores if seed empty.
	if len(out) == 0 {
		for k, vs := range job.Payload {
			v, _ := strconv.ParseFloat(vs, 64)
			if v <= 0 {
				v = 1
			}
			out[k] = v * 0.95
		}
	}
	if w.hook != nil && len(out) > 0 {
		w.hook(out)
	}
	return rec, err
}

func sortedDocIDs(documents map[string]string) []string {
	ids := make([]string, 0, len(documents))
	for id := range documents {
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func utilityPayload(u map[string]float64) map[string]string {
	out := map[string]string{}
	for k, v := range u {
		out[k] = fmt.Sprintf("%g", v)
	}
	return out
}

func edgePayload(e map[string]float64) map[string]string {
	out := map[string]string{}
	for k, v := range e {
		out[k] = fmt.Sprintf("%g", v)
	}
	return out
}

type predictCalibrateWorker struct{}

func (predictCalibrateWorker) Kind() JobKind { return JobPredictCalibrate }
func (predictCalibrateWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	skip := job.Payload["skip_heavy"] == "true"
	out := "run_heavy"
	if skip {
		out = "skip_heavy"
	}
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: out, DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type nremWorker struct{}

func (nremWorker) Kind() JobKind { return JobNREMConsolidate }
func (nremWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	// Deterministic "consolidate": emit shortened header artifact only.
	text := job.Payload["text"]
	art := truncatePayload(text, 120)
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: art, Artifacts: 1, DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type remWorker struct{}

func (remWorker) Kind() JobKind { return JobREMReencode }
func (remWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	// Budget-capped stub: mark reencode without LLM (EnableREM opt-in only).
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: "rem_stub", Artifacts: 1, DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type utilityDecayWorker struct{}

func (utilityDecayWorker) Kind() JobKind { return JobUtilityDecay }
func (utilityDecayWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	// Half-life style decay: multiply each score by 0.95 (deterministic).
	var b strings.Builder
	keys := make([]string, 0, len(job.Payload))
	for k := range job.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := strconv.ParseFloat(job.Payload[k], 64)
		fmt.Fprintf(&b, "%s=%g\n", k, v*0.95)
	}
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: b.String(), DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type hypothesisWorker struct{}

func (hypothesisWorker) Kind() JobKind { return JobHypothesisTest }
func (hypothesisWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	// Downgrade edges with weight < 0.2; strengthen >= 0.2 (sample all).
	//
	// Keys are sorted, as utilityDecayWorker two functions above already does.
	// Iterating the payload map directly made the receipt's Output a different
	// byte sequence on every run for identical input, which defeats receipt
	// digesting and any determinism assertion built on it.
	var b strings.Builder
	keys := make([]string, 0, len(job.Payload))
	for k := range job.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := strconv.ParseFloat(job.Payload[k], 64)
		if v < 0.2 {
			fmt.Fprintf(&b, "downgrade %s\n", k)
		} else {
			fmt.Fprintf(&b, "confirm %s\n", k)
		}
	}
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: b.String(), DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type episodeWorker struct{}

func (episodeWorker) Kind() JobKind { return JobEpisodeReseg }
func (episodeWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: "reseg_ok", DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type pruneWorker struct{}

func (pruneWorker) Kind() JobKind { return JobWeakEdgePrune }
func (pruneWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	n := 0
	for _, vs := range job.Payload {
		v, _ := strconv.ParseFloat(vs, 64)
		if v < 0.1 {
			n++
		}
	}
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: fmt.Sprintf("pruned=%d", n), Artifacts: n, DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}

type gcWorker struct{}

func (gcWorker) Kind() JobKind { return JobGCQuarantine }
func (gcWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	return Receipt{JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID, OK: true, Output: "quarantine_ok", DocumentID: job.DocumentID, FinishedAt: time.Now()}, nil
}
