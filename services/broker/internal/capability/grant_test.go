package capability

import (
	"errors"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestAttenuateIntersectsEveryGrantDimension(t *testing.T) {
	now := time.Unix(100, 0)
	parent := testGrant(now)
	childRequest := Request{
		ID: "child", IDNamespace: parent.IDNamespace, Principal: parent.Principal, Tenant: parent.Tenant,
		PolicyDigest: parent.PolicyDigest,
		Actions:      []string{"artifact.read"}, Resources: parent.Resources,
		AllowedPaths: []string{"src/internal"}, Limits: map[string]uint64{"bytes": 50},
		Fence: 7, RevocationEpoch: 3, ExpiresAt: now.Add(30 * time.Minute), Nonce: "child-nonce",
	}
	child, err := Attenuate(parent, childRequest, now)
	if err != nil {
		t.Fatal(err)
	}
	if child.Limits["bytes"] != 50 || len(child.Actions) != 1 {
		t.Fatalf("child = %#v", child)
	}
	for name, mutate := range map[string]func(*Request){
		"action":      func(request *Request) { request.Actions = []string{"artifact.read", "artifact.delete"} },
		"path":        func(request *Request) { request.AllowedPaths = []string{"other"} },
		"empty_path":  func(request *Request) { request.AllowedPaths = nil },
		"empty_limit": func(request *Request) { request.Limits = nil },
	} {
		t.Run(name, func(t *testing.T) {
			widened := childRequest
			mutate(&widened)
			if _, err := Attenuate(parent, widened, now); !errors.Is(err, ErrDenied) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateGrantRequiresCanonicalIdentityAndPolicyDigest(t *testing.T) {
	now := time.Unix(100, 0)
	valid := testGrant(now)
	if err := ValidateGrant(valid, now); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Grant){
		"namespace": func(grant *Grant) { grant.IDNamespace = "other" },
		"algorithm": func(grant *Grant) { grant.PolicyDigest.Algorithm = "SHA-256" },
		"length":    func(grant *Grant) { grant.PolicyDigest.Hex = grant.PolicyDigest.Hex[:62] },
		"non-hex":   func(grant *Grant) { grant.PolicyDigest.Hex = "z" + grant.PolicyDigest.Hex[1:] },
		"uppercase": func(grant *Grant) { grant.PolicyDigest.Hex = "A" + grant.PolicyDigest.Hex[1:] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := valid
			mutate(&changed)
			if err := ValidateGrant(changed, now); !errors.Is(err, ErrDenied) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestAuthorizeUseRejectsStaleAndMismatchedUseFacts(t *testing.T) {
	now := time.Unix(100, 0)
	grant := testGrant(now)
	identity := contracts.MappedIdentityFact{Principal: grant.Principal, Tenant: grant.Tenant}
	valid := UseRequest{
		Action: "artifact.read", Resource: grant.Resources[0], Fence: 7, RevocationEpoch: 3,
		Nonce: grant.Nonce, Path: "src/file.go", Usage: map[string]uint64{"bytes": 20}, Now: now,
	}
	if err := AuthorizeUse(grant, identity, valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*UseRequest){
		"fence":       func(request *UseRequest) { request.Fence = 6 },
		"epoch":       func(request *UseRequest) { request.RevocationEpoch = 4 },
		"expiry":      func(request *UseRequest) { request.Now = now.Add(2 * time.Hour) },
		"resource":    func(request *UseRequest) { request.Resource.Value = "other" },
		"nonce":       func(request *UseRequest) { request.Nonce = "other" },
		"path":        func(request *UseRequest) { request.Path = "other/file" },
		"usage":       func(request *UseRequest) { request.Usage = map[string]uint64{"bytes": 101} },
		"empty_path":  func(request *UseRequest) { request.Path = "" },
		"empty_usage": func(request *UseRequest) { request.Usage = nil },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			if err := AuthorizeUse(grant, identity, request); !errors.Is(err, ErrDenied) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAuthorizeUseDefinesEmptyPathAndLimitSemantics(t *testing.T) {
	now := time.Unix(100, 0)
	grant := testGrant(now)
	grant.AllowedPaths = nil
	grant.Limits = nil
	identity := contracts.MappedIdentityFact{Principal: grant.Principal, Tenant: grant.Tenant}
	request := UseRequest{Action: "artifact.read", Resource: grant.Resources[0], Fence: 7, RevocationEpoch: 3, Nonce: grant.Nonce, Now: now}
	if err := AuthorizeUse(grant, identity, request); err != nil {
		t.Fatal(err)
	}
	request.Path = "src/file.go"
	if err := AuthorizeUse(grant, identity, request); !errors.Is(err, ErrDenied) {
		t.Fatalf("unbounded path error = %v", err)
	}
	request.Path = ""
	request.Usage = map[string]uint64{"bytes": 1}
	if err := AuthorizeUse(grant, identity, request); !errors.Is(err, ErrDenied) {
		t.Fatalf("unbounded usage error = %v", err)
	}
}

func testGrant(now time.Time) Grant {
	return Grant{
		ID: "parent", IDNamespace: "grant", Principal: contracts.Identifier{Namespace: "principal", Value: "p"},
		Tenant: contracts.Identifier{Namespace: "tenant", Value: "t"},
		PolicyDigest: contracts.Digest{
			Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Actions: []string{"artifact.read"}, Resources: []contracts.Identifier{{Namespace: "artifact", Value: "a"}},
		AllowedPaths: []string{"src"}, Limits: map[string]uint64{"bytes": 100},
		Fence: 7, RevocationEpoch: 3, ExpiresAt: now.Add(time.Hour), Nonce: "parent-nonce",
	}
}
