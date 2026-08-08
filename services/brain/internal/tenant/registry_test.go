package tenant_test

import (
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/tenant"
)

func TestTenantIsolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reg := &tenant.Registry{Root: root}
	if _, err := reg.Create("t1", "us"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create("t2", "eu"); err != nil {
		t.Fatal(err)
	}
	b1 := reg.BrainDir("t1", "brain-a")
	if err := reg.AuthorizeBrainPath("t1", b1); err != nil {
		t.Fatal(err)
	}
	// t2 must not access t1 brain path
	if err := reg.AuthorizeBrainPath("t2", b1); err != tenant.ErrCrossTenant {
		t.Fatalf("got %v", err)
	}
	// Guessable sibling path under other tenant
	other := filepath.Join(root, "tenants", "t1", "brains", "stolen")
	if err := reg.AuthorizeBrainPath("t2", other); err != tenant.ErrCrossTenant {
		t.Fatalf("got %v", err)
	}
	// Sibling prefix: .../brains-evil must not match HasPrefix of .../brains
	evil := filepath.Join(root, "tenants", "t1", "brains-evil", "x")
	if err := reg.AuthorizeBrainPath("t1", evil); err != tenant.ErrCrossTenant {
		t.Fatalf("prefix sibling should deny: %v", err)
	}
	if err := reg.Disable("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Status("t1"); err == nil {
		t.Fatal("disabled should fail status")
	}
}

func TestTenantCreateRejectsPathID(t *testing.T) {
	t.Parallel()
	reg := &tenant.Registry{Root: t.TempDir()}
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, "foo/../bar", "a..b/c"}
	for _, id := range bad {
		if _, err := reg.Create(id, "us"); err == nil {
			t.Fatalf("expected reject for tenant id %q", id)
		}
	}
	if _, err := reg.Create("ok-tenant", "us"); err != nil {
		t.Fatal(err)
	}
}
