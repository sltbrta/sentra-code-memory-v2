package contextpack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const goSource = `package demo

// Alpha is documented.
func Alpha() int {
	return 1
}

func Beta() int {
	return Alpha()
}
`

func TestBudgetByteLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		b    Budget
		want int
	}{
		{Budget{}, 0},
		{Budget{MaxBytes: 100}, 100},
		{Budget{MaxTokens: 10}, 40},
		{Budget{MaxBytes: 100, MaxTokens: 10}, 40},
		{Budget{MaxBytes: 10, MaxTokens: 100}, 10},
	}
	for _, c := range cases {
		if got := c.b.ByteLimit(); got != c.want {
			t.Fatalf("%+v: got %d want %d", c.b, got, c.want)
		}
	}
}

func TestRenderModes(t *testing.T) {
	t.Parallel()
	if got := Render(goSource, RenderFull); got != goSource {
		t.Fatal("full must be identity")
	}
	sig := Render(goSource, RenderSignatures)
	for _, want := range []string{"package demo", "func Alpha() int {", "func Beta() int {"} {
		if !strings.Contains(sig, want) {
			t.Fatalf("signatures missing %q:\n%s", want, sig)
		}
	}
	if strings.Contains(sig, "return 1") || strings.Contains(sig, "// Alpha") {
		t.Fatalf("signatures leaked body/comment:\n%s", sig)
	}
	skel := Render(goSource, RenderSkeleton)
	if !strings.Contains(skel, "func Alpha() int {") || !strings.Contains(skel, "}") {
		t.Fatalf("skeleton must keep decls and closers:\n%s", skel)
	}
	if strings.Contains(skel, "return Alpha()") {
		t.Fatalf("skeleton leaked body:\n%s", skel)
	}
	compact := Render(goSource, RenderCompact)
	if strings.Contains(compact, "// Alpha") {
		t.Fatalf("compact kept comment:\n%s", compact)
	}
	if strings.Contains(compact, "\n\n") {
		t.Fatalf("compact kept blank lines:\n%q", compact)
	}
	if !strings.Contains(compact, "return Alpha()") {
		t.Fatalf("compact must keep body lines:\n%s", compact)
	}
	if _, err := ParseRenderMode("nope"); err == nil {
		t.Fatal("invalid render mode must fail")
	}
	if m, err := ParseRenderMode(""); err != nil || m != RenderFull {
		t.Fatalf("empty render must default full: %v %v", m, err)
	}
}

func TestPackHardBudgetAndAllocation(t *testing.T) {
	t.Parallel()
	body := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("line of filler content number ")
			b.WriteString(strings.Repeat("x", 20))
			b.WriteByte('\n')
		}
		return b.String()
	}
	sources := []Source{
		{Path: "direct.go", Content: body(20), Score: 0.1, Direct: true, StartLine: 1, EndLine: 20},
		{Path: "weak.go", Content: body(20), Score: 0.9, StartLine: 1, EndLine: 20},
	}
	res := Pack(Request{
		Sources: sources,
		Budget:  Budget{MaxBytes: 400},
		Render:  RenderFull,
	})
	if res.Meta.UsedBytes > 400 {
		t.Fatalf("hard byte budget exceeded: %d", res.Meta.UsedBytes)
	}
	if res.Meta.UsedBytes+res.Meta.Unallocated > 400 {
		t.Fatalf("accounting overflow: %+v", res.Meta)
	}
	if res.Meta.Truncated == 0 {
		t.Fatalf("expected truncation: %+v", res.Meta)
	}
	// Direct floor: the low-score direct source must still get floor share
	// (0.4*400 = 160 bytes reserved pool).
	var direct, weak *PackedItem
	for i := range res.Items {
		switch res.Items[i].Path {
		case "direct.go":
			direct = &res.Items[i]
		case "weak.go":
			weak = &res.Items[i]
		}
	}
	if direct == nil || weak == nil {
		t.Fatalf("missing items: %+v", res.Items)
	}
	if direct.Bytes < 100 {
		t.Fatalf("direct floor not honored: %d bytes", direct.Bytes)
	}
	// Proportional: the 9x-scored weak source gets more of the remainder.
	if weak.Bytes <= direct.Bytes {
		t.Fatalf("proportional allocation broken: direct=%d weak=%d", direct.Bytes, weak.Bytes)
	}
	// Line-boundary truncation: content is a whole-line prefix of the source.
	full := body(20)
	if !strings.HasPrefix(full, direct.Content) || full[len(direct.Content)] != '\n' {
		t.Fatalf("truncation cut mid-line: %q", direct.Content[len(direct.Content)-10:])
	}
	// Every emitted item has an expansion handle.
	for _, it := range res.Items {
		if it.Handle == "" || it.HandleKind != string(HandleExpandRange) {
			t.Fatalf("missing handle: %+v", it)
		}
	}
}

func TestPackTokenBudgetBinds(t *testing.T) {
	t.Parallel()
	sources := []Source{{Path: "a.go", Content: strings.Repeat("abcd\n", 80), Score: 1}}
	res := Pack(Request{Sources: sources, Budget: Budget{MaxTokens: 25}, Render: RenderFull})
	if res.Meta.UsedTokens > 25 {
		t.Fatalf("token budget exceeded: %d", res.Meta.UsedTokens)
	}
	if res.Meta.UsedBytes > 100 {
		t.Fatalf("token->byte cap wrong: %d", res.Meta.UsedBytes)
	}
}

func TestPackOmissionHasHandleAndReason(t *testing.T) {
	t.Parallel()
	multi := strings.Repeat("abcdefghij\n", 50) // 550 bytes, 50 lines
	sources := []Source{
		{Path: "a.go", Content: multi, Score: 2},
		{Path: "b.go", Content: multi, Score: 1},
	}
	gov := NewGovernor(Limits{MaxOutputBytes: 100}, nil)
	res := Pack(Request{Sources: sources, Budget: Budget{MaxBytes: 400}, Governor: gov})
	if res.Meta.Omitted == 0 {
		t.Fatalf("expected governor omissions: %+v", res.Meta)
	}
	for _, om := range res.Omitted {
		if om.Handle == "" || om.Reason == "" {
			t.Fatalf("silent omission: %+v", om)
		}
	}
	if res.Meta.Governor == nil || res.Meta.Governor.OutputBytes > 100 {
		t.Fatalf("governor report wrong: %+v", res.Meta.Governor)
	}
}

func TestDedupBackPointers(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	src := []Source{{Path: "a.go", Content: goSource, Score: 1, StartLine: 1, EndLine: 9}}
	first := Pack(Request{Sources: src, Registry: reg})
	second := Pack(Request{Sources: src, Registry: reg})
	if first.Items[0].Dedup {
		t.Fatal("first emission must not dedup")
	}
	it := second.Items[0]
	if !it.Dedup || it.Content != "" || it.Bytes != 0 {
		t.Fatalf("second emission must dedup to a back-pointer: %+v", it)
	}
	if it.DedupRef == "" || it.DedupRef != it.Ref {
		t.Fatalf("back-pointer wrong: %+v", it)
	}
	if second.Meta.Deduped != 1 || second.Meta.DedupSavedByte != len(goSource) {
		t.Fatalf("dedup accounting wrong: %+v", second.Meta)
	}
}

func TestHandlesDeterministicAndStale(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	k := Key{Path: "a.go", StartLine: 1, EndLine: 9}
	h1 := NewHandle(k, goSource)
	h2 := NewHandle(k, goSource)
	if h1.ID != h2.ID {
		t.Fatalf("handles must be deterministic: %s vs %s", h1.ID, h2.ID)
	}
	if _, st := reg.Resolve(h1.ID); st != HandleUnknown {
		t.Fatalf("unissued handle must be unknown: %v", st)
	}
	reg.Issue(h1)
	if dedup, _ := reg.Track(k, h1.Fingerprint); dedup {
		t.Fatal("first track must not dedup")
	}
	if _, st := reg.Resolve(h1.ID); st != HandleOK {
		t.Fatalf("issued+tracked handle must be ok: %v", st)
	}
	// Content drift: new fingerprint tracked, old handle now stale.
	changed := goSource + "\nfunc Gamma() {}\n"
	h3 := NewHandle(k, changed)
	if h3.ID == h1.ID {
		t.Fatal("changed content must change handle ID")
	}
	reg.Issue(h3)
	if dedup, _ := reg.Track(k, h3.Fingerprint); dedup {
		t.Fatal("changed content must not dedup against the old fingerprint")
	}
	if _, st := reg.Resolve(h1.ID); st != HandleStale {
		t.Fatalf("old handle must be stale after drift: %v", st)
	}
	if _, st := reg.Resolve(h3.ID); st != HandleOK {
		t.Fatalf("new handle must be ok: %v", st)
	}
}

func TestPackDeterministicOutput(t *testing.T) {
	t.Parallel()
	mk := func() Result {
		return Pack(Request{
			Sources: []Source{
				{Path: "b.go", Content: goSource, Score: 0.5, StartLine: 1, EndLine: 9},
				{Path: "a.go", Content: goSource, Score: 1, Direct: true, StartLine: 1, EndLine: 9},
				{Path: "c.go", Content: goSource, Score: 0.5, StartLine: 1, EndLine: 9},
			},
			Budget: Budget{MaxBytes: 300, MaxTokens: 100},
			Render: RenderSkeleton,
		})
	}
	a, err := json.Marshal(mk())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(mk())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("non-deterministic pack:\n%s\n%s", a, b)
	}
	// Ordering: score desc, then path asc.
	got := mk()
	if got.Items[0].Path != "a.go" || got.Items[1].Path != "b.go" || got.Items[2].Path != "c.go" {
		t.Fatalf("ordering wrong: %v", []string{got.Items[0].Path, got.Items[1].Path, got.Items[2].Path})
	}
}

func TestGovernorLimitsFailSafe(t *testing.T) {
	t.Parallel()
	// Wall time via injected clock.
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	gov := NewGovernor(Limits{MaxWallTime: time.Second, MaxCandidates: 1, MaxWorkers: 1, MaxOutputBytes: 5}, clock)
	if err := gov.CheckTime(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := gov.CheckTime(); err != ErrLimitExceeded {
		t.Fatalf("wall time must fail closed: %v", err)
	}
	if !gov.AllowCandidate() || gov.AllowCandidate() {
		t.Fatal("candidate limit not enforced")
	}
	if !gov.AcquireWorker() || gov.AcquireWorker() {
		t.Fatal("worker limit not enforced")
	}
	gov.ReleaseWorker()
	gov.ReleaseWorker() // must not go negative
	if !gov.AcquireWorker() {
		t.Fatal("release must free the slot")
	}
	if !gov.ReserveOutputBytes(5) || gov.ReserveOutputBytes(1) {
		t.Fatal("output byte limit not enforced")
	}
	if gov.ReserveOutputBytes(-1) {
		t.Fatal("negative reservation must be denied")
	}
	rep := gov.Report()
	if rep.Limits.MaxCandidates != 1 || rep.Candidates != 1 || rep.OutputBytes != 5 || !rep.WallTimeLimited {
		t.Fatalf("report wrong: %+v", rep)
	}
	// Wall-time limit during Pack omits remaining candidates clearly.
	t2 := time.Unix(2000, 0)
	gov2 := NewGovernor(Limits{MaxWallTime: time.Nanosecond}, func() time.Time { return t2 })
	t2 = t2.Add(time.Hour)
	res := Pack(Request{
		Sources:  []Source{{Path: "a.go", Content: goSource, Score: 1}},
		Governor: gov2,
		Registry: NewRegistry(),
	})
	if res.Meta.Omitted != 1 || res.Omitted[0].Reason != "limit_wall_time" {
		t.Fatalf("wall-time omission wrong: %+v", res.Meta)
	}
}

func TestGovernorTimingIsNotPacked(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	now := start
	gov := NewGovernor(Limits{}, func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	})
	res := Pack(Request{Sources: []Source{{Path: "a.go", Content: "package a\n"}}, Governor: gov})
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "elapsed_ms") || strings.Contains(string(raw), "wall_time_limited") {
		t.Fatalf("runtime governor timing leaked into packed JSON: %s", raw)
	}
}
