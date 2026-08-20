package productsec_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

// Three independent ways existed to get past a multi_principal ACL, all of them
// forms of failing open:
//
//  1. Deleting security.json downgraded the brain to single_user, where
//     Authorize returns nil unconditionally. The file is 0600 but deletable by
//     anyone who can write the brain directory, and nothing bound its absence
//     to anything.
//  2. Omitting a principal substituted the owner, so the ACL was bypassed by
//     *not* presenting an identity -- the opposite of fail-closed.
//  3. Authorize discarded its action argument, so a read grant was equally an
//     ingest and export grant, and there was no way to express read-only.

func multiPrincipalBrain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := productsec.SaveSecurity(dir, productsec.BrainSecurity{
		Profile: productsec.ProfileMultiPrincipal,
		Owner:   "owner@example.invalid",
		Grants:  map[string]bool{"reader@example.invalid": true},
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMissingSecurityFileDoesNotDowngradeAnACLBrain(t *testing.T) {
	dir := multiPrincipalBrain(t)
	if err := os.Remove(filepath.Join(dir, "security.json")); err != nil {
		t.Fatal(err)
	}

	// A brain directory that still holds a corpus but has lost its ACL must not
	// silently become single_user.
	if err := os.WriteFile(filepath.Join(dir, "chunks.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, err := productsec.ContextFromBrain(dir, "mallory@example.invalid", "")
	if err == nil && ctx.Authorize("ask") == nil {
		t.Fatal("deleting security.json downgraded a populated brain to single_user")
	}
}

func TestEmptyPrincipalIsDeniedRatherThanTreatedAsOwner(t *testing.T) {
	dir := multiPrincipalBrain(t)

	ctx, err := productsec.ContextFromBrain(dir, "", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("ask"); !errors.Is(err, productsec.ErrDenied) {
		t.Fatalf("omitting a principal was authorised as the owner (err=%v)", err)
	}
}

func TestGrantedPrincipalIsStillAdmitted(t *testing.T) {
	dir := multiPrincipalBrain(t)
	ctx, err := productsec.ContextFromBrain(dir, "reader@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("ask"); err != nil {
		t.Fatalf("a granted principal must be admitted: %v", err)
	}
}

func TestUngrantedPrincipalIsDenied(t *testing.T) {
	dir := multiPrincipalBrain(t)
	ctx, err := productsec.ContextFromBrain(dir, "mallory@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("ask"); !errors.Is(err, productsec.ErrDenied) {
		t.Fatalf("an ungranted principal must be denied, got %v", err)
	}
}

// TestOwnerRemainsAuthorisedWhenNamedExplicitly keeps the fix from locking the
// owner out of their own brain.
func TestOwnerRemainsAuthorisedWhenNamedExplicitly(t *testing.T) {
	dir := multiPrincipalBrain(t)
	ctx, err := productsec.ContextFromBrain(dir, "owner@example.invalid", "")
	if err != nil {
		t.Fatalf("ContextFromBrain: %v", err)
	}
	if err := ctx.Authorize("ask"); err != nil {
		t.Fatalf("the owner must remain authorised: %v", err)
	}
}
