package productsec_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

// A-003. Authorize discarded its action argument (`_ = action`), so a grant
// was all-or-nothing: whoever could ask could also ingest and export, and a
// read-only grant could not be expressed at all. ActionGrants closes that, and
// nothing referenced it -- reverting the fix left the suite green.
//
// Each case below fails if action is discarded again, in one of two ways: a
// principal held only in ActionGrants is denied everything, or a principal
// granted one action is admitted for the rest.

func actionGrantBrain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := productsec.SaveSecurity(dir, productsec.BrainSecurity{
		Profile: productsec.ProfileMultiPrincipal,
		Owner:   "owner@example.invalid",
		Grants:  map[string]bool{"staff@example.invalid": true},
		ActionGrants: map[string]map[string]bool{
			"reader@example.invalid": {"ask": true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestActionGrantAdmitsOnlyTheNamedAction(t *testing.T) {
	ctx, err := productsec.ContextFromBrain(actionGrantBrain(t), "reader@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("ask"); err != nil {
		t.Fatalf("a read-only principal must still be able to ask: %v", err)
	}
	for _, action := range []string{"ingest", "export"} {
		if err := ctx.Authorize(action); !errors.Is(err, productsec.ErrDenied) {
			t.Fatalf("a read-only grant authorised %q (err=%v): the action argument is being discarded", action, err)
		}
	}
}

// TestActionGrantIsAuthoritativeForThePrincipalsItNames stops a coarse Grants
// entry from silently widening a deliberately narrow one.
func TestActionGrantIsAuthoritativeForThePrincipalsItNames(t *testing.T) {
	dir := t.TempDir()
	if err := productsec.SaveSecurity(dir, productsec.BrainSecurity{
		Profile: productsec.ProfileMultiPrincipal,
		Owner:   "owner@example.invalid",
		// Present in both: the narrow entry has to win, or downgrading a
		// principal to read-only would be a no-op.
		Grants: map[string]bool{"reader@example.invalid": true},
		ActionGrants: map[string]map[string]bool{
			"reader@example.invalid": {"ask": true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, err := productsec.ContextFromBrain(dir, "reader@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("ingest"); !errors.Is(err, productsec.ErrDenied) {
		t.Fatalf("a coarse Grants entry overrode a narrow ActionGrants entry (err=%v)", err)
	}
}

// TestBareGrantStillAdmitsEveryAction pins the compatibility half: existing
// security.json files carry only Grants and must keep working.
func TestBareGrantStillAdmitsEveryAction(t *testing.T) {
	ctx, err := productsec.ContextFromBrain(actionGrantBrain(t), "staff@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	for _, action := range []string{"ask", "ingest", "export"} {
		if err := ctx.Authorize(action); err != nil {
			t.Fatalf("a bare Grants entry must admit %q: %v", action, err)
		}
	}
}

// TestUnnamedActionIsDenied covers the other half of honouring the argument:
// an action that cannot be checked against a grant must not be admitted.
func TestUnnamedActionIsDenied(t *testing.T) {
	ctx, err := productsec.ContextFromBrain(actionGrantBrain(t), "staff@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("  "); !errors.Is(err, productsec.ErrDenied) {
		t.Fatalf("an unnamed action must be denied, got %v", err)
	}
}

// TestActionGrantsSurviveTheSecurityFileRoundTrip catches a narrowing that is
// honoured in memory and dropped on disk, which would widen the grant on the
// next process start.
func TestActionGrantsSurviveTheSecurityFileRoundTrip(t *testing.T) {
	dir := actionGrantBrain(t)
	raw, err := os.ReadFile(filepath.Join(dir, "security.json"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := productsec.LoadSecurity(dir)
	if err != nil {
		t.Fatalf("LoadSecurity: %v", err)
	}
	if !loaded.ActionGrants["reader@example.invalid"]["ask"] {
		t.Fatalf("ActionGrants did not survive the round trip; file was:\n%s", raw)
	}
	if loaded.ActionGrants["reader@example.invalid"]["ingest"] {
		t.Fatal("an action nobody granted came back true")
	}
}
