package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Environment variables for the elevated live OpenFGA path. When the API URL
// is unset the default composition stays on the in-process adapter.
const (
	EnvOpenFGAAPIURL  = "OUROBOROS_OPENFGA_API_URL"
	EnvOpenFGAStoreID = "OUROBOROS_OPENFGA_STORE_ID"
	EnvOpenFGAModelID = "OUROBOROS_OPENFGA_MODEL_ID"
	EnvOpenFGAToken   = "OUROBOROS_OPENFGA_API_TOKEN"
)

// ClientConfig configures an OpenFGA HTTP RelationshipStore.
type ClientConfig struct {
	// APIURL is the OpenFGA base URL (no trailing path), e.g. http://127.0.0.1:8080.
	APIURL string
	// StoreID is the OpenFGA store identifier.
	StoreID string
	// AuthorizationModelID is optional; when set it is sent on Check/Write.
	AuthorizationModelID string
	// APIToken is an optional bearer token.
	APIToken string
	// HTTPClient overrides the default timeout client when non-nil.
	HTTPClient *http.Client
}

// Client is an OpenFGA HTTP adapter. Tuple writes and authorization checks go
// to the remote API; the application deny-epoch overlay and tenant-scope mirror
// stay local so Broker semantics remain fail-closed without a durable store.
//
// Live-server conformance against a real OpenFGA process remains an elevated
// residual (DEF-015). Hermetic dual-run tests use a fixture-compatible fake.
type Client struct {
	apiURL  string
	storeID string
	modelID string
	token   string
	http    *http.Client
	local   *Evaluator
}

// NewClient constructs a remote OpenFGA adapter. APIURL and StoreID are required.
func NewClient(cfg ClientConfig) (*Client, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	storeID := strings.TrimSpace(cfg.StoreID)
	if apiURL == "" || storeID == "" {
		return nil, fmt.Errorf("authz: openfga client requires APIURL and StoreID")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{
		apiURL:  apiURL,
		storeID: storeID,
		modelID: strings.TrimSpace(cfg.AuthorizationModelID),
		token:   strings.TrimSpace(cfg.APIToken),
		http:    httpClient,
		local:   NewEvaluator(),
	}, nil
}

// NewClientFromEnv returns a Client when OUROBOROS_OPENFGA_API_URL is set.
// When the URL is unset, configured is false and callers must keep the
// in-process default. A set URL with a missing store ID is an error.
func NewClientFromEnv() (client *Client, configured bool, err error) {
	apiURL := strings.TrimSpace(os.Getenv(EnvOpenFGAAPIURL))
	if apiURL == "" {
		return nil, false, nil
	}
	storeID := strings.TrimSpace(os.Getenv(EnvOpenFGAStoreID))
	if storeID == "" {
		return nil, true, fmt.Errorf("authz: %s is set but %s is empty", EnvOpenFGAAPIURL, EnvOpenFGAStoreID)
	}
	client, err = NewClient(ClientConfig{
		APIURL:               apiURL,
		StoreID:              storeID,
		AuthorizationModelID: os.Getenv(EnvOpenFGAModelID),
		APIToken:             os.Getenv(EnvOpenFGAToken),
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// Write pushes one relationship tuple to OpenFGA and mirrors it locally.
func (c *Client) Write(tuple Tuple) error {
	if c == nil || !validTuple(tuple) {
		return ErrMalformedTuple
	}
	if err := c.remoteWrite(context.Background(), []Tuple{tuple}, nil); err != nil {
		return err
	}
	return c.local.Write(tuple)
}

// Delete removes one relationship remotely then advances the local deny epoch.
func (c *Client) Delete(tuple Tuple) (string, uint64, error) {
	if c == nil || !validTuple(tuple) {
		return "", 0, ErrMalformedTuple
	}
	if !c.local.hasTuple(tuple) {
		return "", 0, ErrMalformedTuple
	}
	if _, ok := c.local.peekOwningTenant(tuple.Object); !ok {
		return "", 0, ErrMalformedTuple
	}
	if err := c.remoteWrite(context.Background(), nil, []Tuple{tuple}); err != nil {
		return "", 0, err
	}
	return c.local.Delete(tuple)
}

// SetEpoch advances the local application deny epoch only.
func (c *Client) SetEpoch(tenant string, epoch uint64) error {
	if c == nil {
		return ErrMalformedTuple
	}
	return c.local.SetEpoch(tenant, epoch)
}

// Epoch returns the local application deny epoch.
func (c *Client) Epoch(tenant string) (uint64, error) {
	if c == nil {
		return 0, ErrMalformedTuple
	}
	return c.local.Epoch(tenant)
}

// Check evaluates evidence actions via OpenFGA Check plus local epoch/tenant guards.
func (c *Client) Check(ctx context.Context, identity contracts.MappedIdentityFact, request contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	decision := contracts.PolicyDecision{Receipt: deniedReceipt(), Allowed: false}
	if c == nil || c.local == nil {
		return decision, nil
	}
	if identity.Principal.Namespace != "principal" || identity.Principal.Value == "" ||
		identity.Tenant.Namespace != "tenant" || identity.Tenant.Value == "" ||
		request.Resource.Namespace != "evidence" || request.Resource.Value == "" {
		return decision, nil
	}
	current, err := c.local.Epoch(identity.Tenant.Value)
	if err != nil {
		return decision, nil
	}
	decision.RevocationEpoch = current
	if request.RevocationEpoch != current {
		return decision, nil
	}
	relation, ok := evidenceRelationForAction(request.Action)
	if !ok {
		return decision, nil
	}
	evidence := "evidence:" + request.Resource.Value
	if !c.local.evidenceInTenant(evidence, identity.Tenant.Value) {
		return decision, nil
	}
	user := "user:" + identity.Principal.Value
	allowed, err := c.remoteCheck(ctx, user, relation, evidence)
	if err != nil {
		return decision, fmt.Errorf("authz: openfga check: %w", err)
	}
	if allowed {
		decision.Allowed = true
		decision.Receipt.Status = "completed"
		decision.Receipt.ReasonCode = "allowed"
	}
	return decision, nil
}

// CheckSource evaluates source/query/factory actions via OpenFGA Check on the brain.
func (c *Client) CheckSource(ctx context.Context, identity contracts.MappedIdentityFact, action string, brain contracts.Identifier) (contracts.PolicyDecision, error) {
	decision := contracts.PolicyDecision{Receipt: deniedReceipt(), Allowed: false}
	if c == nil || c.local == nil || identity.Principal.Namespace != "principal" || identity.Principal.Value == "" ||
		identity.Tenant.Namespace != "tenant" || identity.Tenant.Value == "" ||
		brain.Namespace != "brain" || brain.Value == "" {
		return decision, nil
	}
	epoch, err := c.local.Epoch(identity.Tenant.Value)
	if err != nil {
		return decision, nil
	}
	decision.RevocationEpoch = epoch
	brainObject := "brain:" + brain.Value
	if !c.local.brainInTenant(brainObject, identity.Tenant.Value) {
		return decision, nil
	}
	relation, ok := brainRelationForSourceAction(action)
	if !ok {
		return decision, nil
	}
	user := "user:" + identity.Principal.Value
	allowed, err := c.remoteCheck(ctx, user, relation, brainObject)
	if err != nil {
		return decision, fmt.Errorf("authz: openfga check: %w", err)
	}
	if allowed {
		decision.Allowed = true
		decision.Receipt.Status = "completed"
		decision.Receipt.ReasonCode = "allowed"
	}
	return decision, nil
}

func evidenceRelationForAction(action string) (string, bool) {
	switch action {
	case "artifact.read":
		return "reader", true
	case "artifact.admit":
		return "admittor", true
	case "artifact.delete":
		return "deleter", true
	default:
		return "", false
	}
}

func brainRelationForSourceAction(action string) (string, bool) {
	switch action {
	case "source.add", "source.reconcile", "source.revoke",
		"factory.admit", "factory.cancel", "file.read", "file.write":
		return "writer", true
	case "source.status", "source.search", "query", "hydrate", "emit":
		return "reader", true
	default:
		return "", false
	}
}

type openfgaTupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type openfgaWriteBody struct {
	Writes               *openfgaTupleKeys `json:"writes,omitempty"`
	Deletes              *openfgaTupleKeys `json:"deletes,omitempty"`
	AuthorizationModelID string            `json:"authorization_model_id,omitempty"`
}

type openfgaTupleKeys struct {
	TupleKeys []openfgaTupleKey `json:"tuple_keys"`
}

type openfgaCheckBody struct {
	TupleKey             openfgaTupleKey `json:"tuple_key"`
	AuthorizationModelID string          `json:"authorization_model_id,omitempty"`
}

type openfgaCheckResponse struct {
	Allowed bool `json:"allowed"`
}

func (c *Client) remoteWrite(ctx context.Context, writes, deletes []Tuple) error {
	body := openfgaWriteBody{AuthorizationModelID: c.modelID}
	if len(writes) > 0 {
		keys := make([]openfgaTupleKey, 0, len(writes))
		for _, tuple := range writes {
			keys = append(keys, openfgaTupleKey{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object})
		}
		body.Writes = &openfgaTupleKeys{TupleKeys: keys}
	}
	if len(deletes) > 0 {
		keys := make([]openfgaTupleKey, 0, len(deletes))
		for _, tuple := range deletes {
			keys = append(keys, openfgaTupleKey{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object})
		}
		body.Deletes = &openfgaTupleKeys{TupleKeys: keys}
	}
	return c.postJSON(ctx, "/stores/"+c.storeID+"/write", body, nil)
}

func (c *Client) remoteCheck(ctx context.Context, user, relation, object string) (bool, error) {
	body := openfgaCheckBody{
		TupleKey:             openfgaTupleKey{User: user, Relation: relation, Object: object},
		AuthorizationModelID: c.modelID,
	}
	var resp openfgaCheckResponse
	if err := c.postJSON(ctx, "/stores/"+c.storeID+"/check", body, &resp); err != nil {
		return false, err
	}
	return resp.Allowed, nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("openfga http %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

var _ RelationshipStore = (*Client)(nil)
