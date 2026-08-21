package authorityprocess

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
)

// The BUILD gate checked that every leaf reached COMPLETED -- a property of
// the executor, not of the code it produced -- and the TEST gate checked that
// touched Go files parse, which is strictly weaker than compiling: an
// undefined symbol, a type error, a wrong signature and a missing import all
// parse. A change set touching no Go file skipped both and passed having been
// checked by nothing.
//
// Every case below is one of those: each candidate parses, and each is
// something the old gates called PASSED.

// toolchainModule writes a small module that builds and whose test passes, and
// returns its root. The edits in each case are applied over it through an
// overlay, so the module on disk is never modified.
func toolchainModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module candidate\n\ngo 1.21\n")
	write("calc/calc.go", `package calc

// Double returns twice n.
func Double(n int) int { return n * 2 }
`)
	write("calc/assets/version.txt", "v1\n")
	write("calc/embed.go", `package calc

import _ "embed"

//go:embed assets/version.txt
var version string

// Version reports the embedded version.
func Version() string { return version }
`)
	write("calc/calc_test.go", `package calc

import "testing"

func TestDouble(t *testing.T) {
	if Double(3) != 6 {
		t.Fatal("Double is wrong")
	}
}
`)
	return root
}

func toolchainFor(t *testing.T, root string) factoryToolchain {
	t.Helper()
	return factoryToolchain{repoRoot: root, timeout: 4 * time.Minute}
}

func completedLeaf(edits ...broker.FactoryAppliedEdit) []factoryLeafOutcome {
	return []factoryLeafOutcome{{outcome: broker.FactoryLeafOutcome{
		State: "COMPLETED", Edits: edits,
	}}}
}

func goEdit(path, body string) broker.FactoryAppliedEdit {
	return broker.FactoryAppliedEdit{
		Op: "modify", Path: path, Language: "go", AfterBytes: []byte(body),
	}
}

// TestBuildGateRejectsCodeThatParsesButDoesNotCompile is the finding. Every
// candidate here is syntactically valid Go, so the old gate passed all of
// them.
func TestBuildGateRejectsCodeThatParsesButDoesNotCompile(t *testing.T) {
	root := toolchainModule(t)
	toolchain := toolchainFor(t, root)
	ctx := context.Background()

	for name, body := range map[string]string{
		"undefined symbol": `package calc

// Double returns twice n.
func Double(n int) int { return multiply(n, 2) }
`,
		"type error": `package calc

// Double returns twice n.
func Double(n int) int { return "two" }
`,
		"missing import": `package calc

// Double returns twice n.
func Double(n int) int {
	fmt.Println(n)
	return n * 2
}
`,
	} {
		t.Run(name, func(t *testing.T) {
			edits := completedLeaf(goEdit("calc/calc.go", body))
			if evaluateFactoryGate(ctx, toolchain,
				contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, edits) {
				t.Fatalf("BUILD passed on a candidate that does not compile (%s); "+
					"it parses, which is all the gate used to check", name)
			}
		})
	}
}

func TestBuildGateAcceptsACandidateThatCompiles(t *testing.T) {
	root := toolchainModule(t)
	edits := completedLeaf(goEdit("calc/calc.go", `package calc

// Double returns twice n.
func Double(n int) int { return n + n }
`))
	if !evaluateFactoryGate(context.Background(), toolchainFor(t, root),
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, edits) {
		t.Fatal("BUILD failed on a candidate that compiles")
	}
}

// TestTestGateRejectsACandidateWhoseTestsFail is the other half: the candidate
// compiles, so a build gate alone admits it.
func TestTestGateRejectsACandidateWhoseTestsFail(t *testing.T) {
	root := toolchainModule(t)
	toolchain := toolchainFor(t, root)
	ctx := context.Background()
	edits := completedLeaf(goEdit("calc/calc.go", `package calc

// Double returns twice n.
func Double(n int) int { return n * 3 }
`))

	if !evaluateFactoryGate(ctx, toolchain,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, edits) {
		t.Fatal("the fixture must compile, or this case is about the wrong thing")
	}
	if evaluateFactoryGate(ctx, toolchain,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, edits) {
		t.Fatal("TEST passed on a candidate whose tests fail")
	}
}

func TestTestGateAcceptsACandidateWhoseTestsPass(t *testing.T) {
	root := toolchainModule(t)
	edits := completedLeaf(goEdit("calc/calc.go", `package calc

// Double returns twice n.
func Double(n int) int { return n + n }
`))
	if !evaluateFactoryGate(context.Background(), toolchainFor(t, root),
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, edits) {
		t.Fatal("TEST failed on a candidate whose tests pass")
	}
}

// TestGatesCheckNonGoEditsToo covers the case the old gates skipped entirely:
// a change set that touches no Go file was checked by nothing at all and
// passed. The module is now built and tested regardless of what the edit
// touched, so a non-Go edit that breaks the build is caught.
func TestGatesCheckNonGoEditsToo(t *testing.T) {
	root := toolchainModule(t)
	toolchain := toolchainFor(t, root)
	ctx := context.Background()

	// Deleting an embedded asset breaks the build and touches no Go source at
	// all, so under the old gates this change set was checked by nothing.
	edits := completedLeaf(broker.FactoryAppliedEdit{
		Op: "delete", Path: "calc/assets/version.txt", Language: "text",
	})
	if evaluateFactoryGate(ctx, toolchain,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, edits) {
		t.Fatal("BUILD passed on a change set that breaks the module and " +
			"touches no Go file: this is the case that used to skip the gate")
	}
}

// TestGatesFailClosedWithoutAToolchain pins the default. An unconfigured
// deployment must not report a pass it did not earn -- which is the state the
// whole surface was in.
func TestGatesFailClosedWithoutAToolchain(t *testing.T) {
	edits := completedLeaf(goEdit("calc/calc.go", "package calc\n"))
	for _, kind := range []contractsv1.FactoryGateKind{
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
	} {
		if evaluateFactoryGate(context.Background(), factoryToolchain{}, kind, edits) {
			t.Fatalf("%v passed with no repository root configured", kind)
		}
	}
}

// TestOverlayDeletesRemovedPaths covers the overlay's handling of a change set
// that removes a file: it must be compiled without it, not with it.
//
// The assertion is on TEST rather than BUILD because `go build` does not
// compile test files, so a package left holding only its tests builds cleanly.
// That is the toolchain's behaviour rather than a gap in the gate -- but it is
// why both gates are run rather than BUILD being treated as a superset.
func TestOverlayDeletesRemovedPaths(t *testing.T) {
	root := toolchainModule(t)
	toolchain := toolchainFor(t, root)

	edits := completedLeaf(broker.FactoryAppliedEdit{
		Op: "delete", Path: "calc/calc.go", Language: "go",
	})
	if evaluateFactoryGate(context.Background(), toolchain,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, edits) {
		t.Fatal("TEST passed with the file under test deleted: the overlay is " +
			"compiling the original rather than the candidate")
	}
}

// TestTestGateRejectsASignatureTheCallersDoNotMatch is the wrong-signature
// case. It belongs to TEST rather than BUILD for the reason above: the only
// caller in this fixture is the test.
func TestTestGateRejectsASignatureTheCallersDoNotMatch(t *testing.T) {
	root := toolchainModule(t)
	edits := completedLeaf(goEdit("calc/calc.go", `package calc

// Double returns twice n.
func Double() int { return 2 }
`))
	if evaluateFactoryGate(context.Background(), toolchainFor(t, root),
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, edits) {
		t.Fatal("TEST passed on a candidate whose callers no longer compile")
	}
}

// TestOverlayRefusesAPathOutsideTheRepository keeps the gate from being told
// to compile a file the change set does not own.
func TestOverlayRefusesAPathOutsideTheRepository(t *testing.T) {
	root := toolchainModule(t)
	toolchain := toolchainFor(t, root)

	for _, path := range []string{"../outside.go", "/etc/passwd"} {
		_, _, err := toolchain.writeOverlay([]broker.FactoryAppliedEdit{
			goEdit(path, "package calc\n"),
		})
		if err == nil {
			t.Fatalf("overlay accepted %q", path)
		}
		if !strings.Contains(err.Error(), "escapes the repository") &&
			!strings.Contains(err.Error(), "absolute edit path") {
			t.Fatalf("unexpected refusal for %q: %v", path, err)
		}
	}
}

// TestToolchainDoesNotModifyTheRepository is the property the overlay exists
// for: a rejected candidate must leave nothing behind.
func TestToolchainDoesNotModifyTheRepository(t *testing.T) {
	root := toolchainModule(t)
	before, err := os.ReadFile(filepath.Join(root, "calc", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}

	edits := completedLeaf(goEdit("calc/calc.go", "package calc\n\nfunc Double() {}\n"))
	_ = evaluateFactoryGate(context.Background(), toolchainFor(t, root),
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, edits)

	after, err := os.ReadFile(filepath.Join(root, "calc", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the gate wrote the candidate into the repository")
	}
}
