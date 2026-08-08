package localauthority

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBrokerMapsPeerAndReauthorizesCurrentPolicy(t *testing.T) {
	now := time.Unix(1_000, 0)
	broker := newTestBroker(t)
	mapped, err := broker.MapPeer(PeerCredentials{UID: 501, PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range []string{
		"brain:b#tenant@tenant:t",
		"brain:b#owner@user:p",
		"evidence:a#brain@brain:b",
	} {
		if err := broker.AddRelationship(tuple); err != nil {
			t.Fatal(err)
		}
	}
	grant := Grant{
		ID: "g", IDNamespace: "grant", Principal: mapped.Principal, Tenant: mapped.Tenant,
		PolicyDigest: Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Actions:      []string{"artifact.read"}, Resources: []Identifier{{Namespace: "evidence", Value: "a"}},
		Fence: 7, ExpiresAt: now.Add(time.Minute), Nonce: "n",
	}
	if err := broker.RegisterGrant(grant, now); err != nil {
		t.Fatal(err)
	}
	use := NewUse("artifact.read", grant.Resources[0], 7, 0, "n", now)
	decision, err := broker.Authorize(context.Background(), mapped, grant, use)
	if err != nil || !decision.Allowed {
		t.Fatalf("authorize = %#v, %v", decision, err)
	}
	epoch, err := broker.RemoveRelationship("brain:b#owner@user:p")
	if err != nil || epoch != 1 {
		t.Fatalf("revoke = %d, %v", epoch, err)
	}
	if _, err := broker.Authorize(context.Background(), mapped, grant, use); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale authorization error = %v", err)
	}
}

func TestBrokerAuthorizesCurrentSourceActions(t *testing.T) {
	broker := newTestBroker(t)
	identity, err := broker.MapPeer(PeerCredentials{UID: 501, PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range []string{
		"brain:b#tenant@tenant:t", "brain:b#owner@user:p", "brain:b#viewer@user:viewer",
	} {
		if err := broker.AddRelationship(tuple); err != nil {
			t.Fatal(err)
		}
	}
	brain := Identifier{Namespace: "brain", Value: "b"}
	for _, action := range []string{"source.add", "source.status", "source.search", "source.reconcile", "source.revoke"} {
		decision, err := broker.AuthorizeSource(context.Background(), identity, action, brain)
		if err != nil || !decision.Allowed {
			t.Fatalf("%s = %#v, %v", action, decision, err)
		}
	}
	viewer := identity
	viewer.Principal.Value = "viewer"
	if decision, err := broker.AuthorizeSource(context.Background(), viewer, "source.add", brain); err == nil || decision.Allowed {
		t.Fatalf("viewer mutation = %#v, %v", decision, err)
	}
	if decision, err := broker.AuthorizeSource(context.Background(), viewer, "source.search", brain); err != nil || !decision.Allowed {
		t.Fatalf("viewer search = %#v, %v", decision, err)
	}
	if _, err := broker.RemoveRelationship("brain:b#owner@user:p"); err != nil {
		t.Fatal(err)
	}
	if decision, err := broker.AuthorizeSource(context.Background(), identity, "source.status", brain); err == nil || decision.Allowed {
		t.Fatalf("removed owner = %#v, %v", decision, err)
	}
}

func TestBrokerDeniesWrongPeerAndAbsentRelationshipIndistinguishably(t *testing.T) {
	broker := newTestBroker(t)
	if _, err := broker.MapPeer(PeerCredentials{UID: 502, PID: 42}); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong uid error = %v", err)
	}
	mapped, err := broker.MapPeer(PeerCredentials{UID: 501, PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	grant := Grant{
		ID: "g", IDNamespace: "grant", Principal: mapped.Principal, Tenant: mapped.Tenant,
		PolicyDigest: Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Actions:      []string{"artifact.read"}, Resources: []Identifier{{Namespace: "evidence", Value: "missing"}},
		Fence: 7, ExpiresAt: now.Add(time.Minute), Nonce: "n",
	}
	if err := broker.RegisterGrant(grant, now); err != nil {
		t.Fatal(err)
	}
	_, absent := broker.Authorize(context.Background(), mapped, grant, NewUse(
		"artifact.read", grant.Resources[0], 7, 0, "n", now,
	))
	if !errors.Is(absent, ErrDenied) {
		t.Fatalf("absent error = %v", absent)
	}
}

func TestBrokerRequiresExactTrustedIssuedGrant(t *testing.T) {
	now := time.Unix(1_000, 0)
	broker := newTestBroker(t)
	mapped, err := broker.MapPeer(PeerCredentials{UID: 501, PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	grant := Grant{
		ID: "g", IDNamespace: "grant", Principal: mapped.Principal, Tenant: mapped.Tenant,
		PolicyDigest: Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Actions:      []string{"artifact.read"}, Resources: []Identifier{{Namespace: "evidence", Value: "a"}},
		Limits: map[string]uint64{"bytes": 8}, Fence: 7, RevocationEpoch: 2,
		ExpiresAt: now.Add(time.Minute), Nonce: "n",
	}
	use := NewUse("artifact.read", grant.Resources[0], 7, 2, "n", now)
	use.Usage = map[string]uint64{"bytes": 7}
	if _, err := broker.Authorize(context.Background(), mapped, grant, use); !errors.Is(err, ErrDenied) {
		t.Fatalf("absent issued grant error = %v", err)
	}
	if err := broker.RegisterGrant(grant, now); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Grant){
		"id":           func(value *Grant) { value.ID = "other" },
		"id namespace": func(value *Grant) { value.IDNamespace = "other" },
		"principal":    func(value *Grant) { value.Principal.Value = "other" },
		"tenant":       func(value *Grant) { value.Tenant.Value = "other" },
		"policy digest": func(value *Grant) {
			value.PolicyDigest.Hex = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"actions":          func(value *Grant) { value.Actions = []string{"artifact.delete"} },
		"resources":        func(value *Grant) { value.Resources[0].Value = "other" },
		"paths":            func(value *Grant) { value.AllowedPaths = []string{"src"} },
		"limits":           func(value *Grant) { value.Limits["bytes"] = 9 },
		"fence":            func(value *Grant) { value.Fence++ },
		"revocation epoch": func(value *Grant) { value.RevocationEpoch++ },
		"expiry":           func(value *Grant) { value.ExpiresAt = value.ExpiresAt.Add(time.Second) },
		"nonce":            func(value *Grant) { value.Nonce = "other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := cloneGrant(grant)
			mutate(&changed)
			if _, err := broker.Authorize(context.Background(), mapped, changed, use); !errors.Is(err, ErrDenied) {
				t.Fatalf("mutated grant error = %v", err)
			}
		})
	}
}

func newTestBroker(t *testing.T) *Broker {
	t.Helper()
	broker, err := New(Config{
		UID:       501,
		Principal: Identifier{Namespace: "principal", Value: "p"},
		Tenant:    Identifier{Namespace: "tenant", Value: "t"},
		Session:   Identifier{Namespace: "session", Value: "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return broker
}
