package authz

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// dualFixture is the shared personal/company OpenFGA fixture shape.
type dualFixture struct {
	Tuples []string `json:"tuples"`
	Checks []struct {
		ID          string `json:"id"`
		User        string `json:"user"`
		Relation    string `json:"relation"`
		Object      string `json:"object"`
		Tenant      string `json:"tenant"`
		Allowed     bool   `json:"allowed"`
		AfterRemove string `json:"after_remove"`
	} `json:"checks"`
}

func TestDualRunFixturesInProcessAndHTTPAdapter(t *testing.T) {
	for _, name := range []string{"fixtures.json", "fixtures_company.json"} {
		name := name
		t.Run(name, func(t *testing.T) {
			cases := loadDualFixture(t, name)
			inproc := NewInProcessAdapter()
			client := newHermeticClient(t)

			runFixtureOnStore(t, "in_process", inproc, cases, defaultTenant(name))
			runFixtureOnStore(t, "http_adapter", client, cases, defaultTenant(name))
		})
	}
}

func TestDualRunLiveOpenFGAWhenConfigured(t *testing.T) {
	client, configured, err := NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Skip("OUROBOROS_OPENFGA_API_URL unset; live elevated path residual")
	}
	cases := loadDualFixture(t, "fixtures_company.json")
	runFixtureOnStore(t, "live_openfga", client, cases, "company-acme")
}

func TestClientFromEnvUnconfigured(t *testing.T) {
	t.Setenv(EnvOpenFGAAPIURL, "")
	t.Setenv(EnvOpenFGAStoreID, "")
	client, configured, err := NewClientFromEnv()
	if err != nil || configured || client != nil {
		t.Fatalf("client=%v configured=%v err=%v", client, configured, err)
	}
}

func TestClientFromEnvRequiresStoreID(t *testing.T) {
	t.Setenv(EnvOpenFGAAPIURL, "http://127.0.0.1:9")
	t.Setenv(EnvOpenFGAStoreID, "")
	_, configured, err := NewClientFromEnv()
	if !configured || err == nil {
		t.Fatalf("configured=%v err=%v", configured, err)
	}
}

func TestClientFailsClosedOnTransportError(t *testing.T) {
	client, err := NewClient(ClientConfig{APIURL: "http://127.0.0.1:1", StoreID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Write(mustTuple(t, "brain:b#owner@user:p")); err == nil {
		t.Fatal("expected write transport failure")
	}
	// Seed local mirror only via constructor path is empty; Check still denies.
	decision, err := client.Check(context.Background(), identityFactFor("p", "t"), contracts.PolicyRequest{
		Action: "artifact.read", Resource: contracts.Identifier{Namespace: "evidence", Value: "a"},
	})
	if err != nil {
		// Transport not reached when tenant/evidence guards fail first — also fine.
		return
	}
	if decision.Allowed {
		t.Fatal("transport-unavailable path must not allow")
	}
}

func defaultTenant(fixtureName string) string {
	if strings.Contains(fixtureName, "company") {
		return "company-acme"
	}
	return "personal"
}

func runFixtureOnStore(t *testing.T, label string, store RelationshipStore, cases dualFixture, defaultTenant string) {
	t.Helper()
	for _, raw := range cases.Tuples {
		tuple, err := ParseTuple(raw)
		if err != nil {
			t.Fatalf("%s parse %q: %v", label, raw, err)
		}
		if err := store.Write(tuple); err != nil {
			t.Fatalf("%s write %q: %v", label, raw, err)
		}
	}
	for _, check := range cases.Checks {
		if check.AfterRemove != "" {
			tuple, err := ParseTuple(check.AfterRemove)
			if err != nil {
				t.Fatalf("%s parse remove %q: %v", label, check.AfterRemove, err)
			}
			if _, _, err := store.Delete(tuple); err != nil {
				t.Fatalf("%s delete %q: %v", label, check.AfterRemove, err)
			}
		}
		principal := check.User[len("user:"):]
		resource := check.Object[len("evidence:"):]
		tenant := check.Tenant
		if tenant == "" {
			tenant = defaultTenant
		}
		action := map[string]string{
			"reader": "artifact.read", "admittor": "artifact.admit", "deleter": "artifact.delete",
		}[check.Relation]
		epoch, err := store.Epoch(tenant)
		if err != nil {
			t.Fatalf("%s epoch: %v", label, err)
		}
		decision, err := store.Check(context.Background(), identityFactFor(principal, tenant), contracts.PolicyRequest{
			Action: action, Resource: contracts.Identifier{Namespace: "evidence", Value: resource}, RevocationEpoch: epoch,
		})
		if err != nil {
			t.Fatalf("%s check %s: %v", label, check.ID, err)
		}
		if decision.Allowed != check.Allowed {
			t.Fatalf("%s %s allowed = %v, want %v", label, check.ID, decision.Allowed, check.Allowed)
		}
	}
}

func loadDualFixture(t *testing.T, name string) dualFixture {
	t.Helper()
	data, err := readFixtureBytes(name)
	if err != nil {
		t.Fatal(err)
	}
	var cases dualFixture
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.Tuples) == 0 || len(cases.Checks) == 0 {
		t.Fatalf("empty fixture %s", name)
	}
	return cases
}

func readFixtureBytes(name string) ([]byte, error) {
	candidates := []string{
		filepath.Join("..", "..", "..", "..", "deploy", "openfga", "local", name),
		filepath.Join("deploy", "openfga", "local", name),
	}
	if root := os.Getenv("TEST_SRCDIR"); root != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace == "" {
			workspace = "_main"
		}
		candidates = append(candidates, filepath.Join(root, workspace, "deploy", "openfga", "local", name))
	}
	var last error
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		last = err
	}
	return nil, last
}

func newHermeticClient(t *testing.T) *Client {
	t.Helper()
	server := newFakeOpenFGAServer("store-hermetic")
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	client, err := NewClient(ClientConfig{APIURL: ts.URL, StoreID: "store-hermetic", HTTPClient: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func mustTuple(t *testing.T, raw string) Tuple {
	t.Helper()
	tuple, err := ParseTuple(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tuple
}

// fakeOpenFGAServer speaks enough OpenFGA Check/Write HTTP for dual-run fixtures.
// It is a hermetic test double, not a production OpenFGA implementation.
type fakeOpenFGAServer struct {
	storeID string
	mu      sync.Mutex
	tuples  map[Tuple]struct{}
}

func newFakeOpenFGAServer(storeID string) *fakeOpenFGAServer {
	return &fakeOpenFGAServer{storeID: storeID, tuples: make(map[Tuple]struct{})}
}

func (f *fakeOpenFGAServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	prefix := "/stores/" + f.storeID + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "store", http.StatusNotFound)
		return
	}
	op := strings.TrimPrefix(r.URL.Path, prefix)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	switch op {
	case "write":
		f.handleWrite(w, body)
	case "check":
		f.handleCheck(w, body)
	default:
		http.Error(w, "op", http.StatusNotFound)
	}
}

func (f *fakeOpenFGAServer) handleWrite(w http.ResponseWriter, body []byte) {
	var req openfgaWriteBody
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Writes != nil {
		for _, key := range req.Writes.TupleKeys {
			tuple := Tuple{Object: key.Object, Relation: key.Relation, User: key.User}
			if !validTuple(tuple) {
				http.Error(w, "tuple", http.StatusBadRequest)
				return
			}
			f.tuples[tuple] = struct{}{}
		}
	}
	if req.Deletes != nil {
		for _, key := range req.Deletes.TupleKeys {
			tuple := Tuple{Object: key.Object, Relation: key.Relation, User: key.User}
			if _, ok := f.tuples[tuple]; !ok {
				http.Error(w, "missing", http.StatusBadRequest)
				return
			}
			delete(f.tuples, tuple)
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (f *fakeOpenFGAServer) handleCheck(w http.ResponseWriter, body []byte) {
	var req openfgaCheckBody
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	allowed := f.checkLocked(req.TupleKey.User, req.TupleKey.Relation, req.TupleKey.Object)
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(openfgaCheckResponse{Allowed: allowed})
}

func (f *fakeOpenFGAServer) checkLocked(user, relation, object string) bool {
	entityType, _, ok := splitEntity(object)
	if !ok || user == "" || relation == "" {
		return false
	}
	switch entityType {
	case "evidence":
		switch relation {
		case "reader":
			return f.anyBrain(object, func(brain string) bool {
				return f.has(brain, "owner", user) || f.has(brain, "viewer", user)
			})
		case "admittor", "deleter":
			return f.anyBrain(object, func(brain string) bool {
				return f.has(brain, "owner", user)
			})
		}
	case "brain":
		switch relation {
		case "owner":
			return f.has(object, "owner", user)
		case "viewer":
			return f.has(object, "viewer", user)
		case "reader":
			return f.has(object, "owner", user) || f.has(object, "viewer", user)
		case "writer":
			return f.has(object, "owner", user)
		}
	case "tenant":
		if relation == "member" {
			return f.has(object, "member", user)
		}
	}
	return false
}

func (f *fakeOpenFGAServer) has(object, relation, user string) bool {
	_, ok := f.tuples[Tuple{Object: object, Relation: relation, User: user}]
	return ok
}

func (f *fakeOpenFGAServer) anyBrain(evidence string, pred func(brain string) bool) bool {
	for tuple := range f.tuples {
		if tuple.Object == evidence && tuple.Relation == "brain" {
			if strings.HasPrefix(tuple.User, "brain:") && pred(tuple.User) {
				return true
			}
		}
	}
	return false
}
