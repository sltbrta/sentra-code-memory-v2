package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// HTTPDoer is the narrow HTTP surface the broker uses for GitHub REST calls.
type HTTPDoer interface {
	// Do executes one HTTP request.
	Do(req *http.Request) (*http.Response, error)
}

// API is the provider surface used by the two-phase broker. Production wraps
// the GitHub REST API; tests inject FakeAPI.
type API interface {
	// GetRef returns the object SHA for one ref, or ("", false, nil) when absent.
	GetRef(ctx context.Context, owner, repo, ref string) (sha string, ok bool, err error)
	// CreateRef creates one refs/heads/... at sha. Conflicts when the ref exists.
	CreateRef(ctx context.Context, owner, repo, ref, sha string) error
	// ListPullRequests returns PRs matching head (owner:branch) and base.
	ListPullRequests(ctx context.Context, owner, repo, head, base string) ([]PullRequest, error)
	// CreatePullRequest creates one draft PR. head is the branch name (no refs/).
	CreatePullRequest(ctx context.Context, owner, repo string, in CreatePRInput) (PullRequest, error)
}

// PullRequest is the provider PR projection the broker reconciles against.
type PullRequest struct {
	// Number is the provider PR number (string form for receipt identity).
	Number int
	// NodeID is the provider node identity when available.
	NodeID string
	// HeadRef is the head branch name without refs/heads/.
	HeadRef string
	// BaseRef is the base branch name.
	BaseRef string
	// HeadSHA is the head commit OID.
	HeadSHA string
	// BaseSHA is the base commit OID observed at list time.
	BaseSHA string
	// Draft is true for draft PRs.
	Draft bool
	// State is open or closed.
	State string
	// Title and Body bind content digests.
	Title string
	Body  string
}

// CreatePRInput is the draft PR create body.
type CreatePRInput struct {
	// Title, Body, Head, Base are the PR fields.
	Title string
	Body  string
	Head  string
	Base  string
	// Draft must be true.
	Draft bool
}

// FakeAPI is a deterministic in-process GitHub stand-in for unit tests. It
// never opens sockets. Concurrent creates for the same head+base converge to
// one PR when the second caller lists first (lookup-before-create).
type FakeAPI struct {
	mu     sync.Mutex
	refs   map[string]string // "owner/repo#refs/heads/x" -> sha
	prs    []PullRequest
	nextPR int32
	// FailCreateOnce, when >0, fails the next CreatePullRequest that many times.
	FailCreateOnce int32
	// RejectCreate permanently rejects creates.
	RejectCreate bool
	// CreateCalls counts CreatePullRequest invocations.
	CreateCalls int32
	// CreateRefCalls counts CreateRef invocations.
	CreateRefCalls int32
}

// NewFakeAPI returns an empty fake provider.
func NewFakeAPI() *FakeAPI {
	return &FakeAPI{
		refs:   make(map[string]string),
		nextPR: 1000,
	}
}

// SeedRef installs a ref at sha (test helper).
func (f *FakeAPI) SeedRef(owner, repo, ref, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[refKey(owner, repo, ref)] = sha
}

// SeedPR installs an existing PR (test helper).
func (f *FakeAPI) SeedPR(pr PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr.Number == 0 {
		pr.Number = int(atomic.AddInt32(&f.nextPR, 1))
	}
	f.prs = append(f.prs, pr)
}

// GetRef implements API.
func (f *FakeAPI) GetRef(_ context.Context, owner, repo, ref string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha, ok := f.refs[refKey(owner, repo, ref)]
	return sha, ok, nil
}

// CreateRef implements API.
func (f *FakeAPI) CreateRef(_ context.Context, owner, repo, ref, sha string) error {
	atomic.AddInt32(&f.CreateRefCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := refKey(owner, repo, ref)
	if existing, ok := f.refs[key]; ok {
		if existing == sha {
			return nil // idempotent equal create
		}
		return fmt.Errorf("%w: ref exists at different sha", ErrConflict)
	}
	f.refs[key] = sha
	return nil
}

// ListPullRequests implements API.
func (f *FakeAPI) ListPullRequests(_ context.Context, owner, repo, head, base string) ([]PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PullRequest
	for _, pr := range f.prs {
		// head is "owner:branch" form from the GitHub API convention.
		wantHead := head
		if strings.Contains(head, ":") {
			wantHead = strings.SplitN(head, ":", 2)[1]
		}
		if pr.HeadRef == wantHead && pr.BaseRef == base {
			out = append(out, pr)
		}
	}
	_ = owner
	_ = repo
	return out, nil
}

// CreatePullRequest implements API.
func (f *FakeAPI) CreatePullRequest(_ context.Context, owner, repo string, in CreatePRInput) (PullRequest, error) {
	atomic.AddInt32(&f.CreateCalls, 1)
	if f.RejectCreate {
		return PullRequest{}, fmt.Errorf("github: create rejected")
	}
	if remaining := atomic.LoadInt32(&f.FailCreateOnce); remaining > 0 {
		if atomic.AddInt32(&f.FailCreateOnce, -1) >= 0 {
			return PullRequest{}, fmt.Errorf("github: simulated 5xx")
		}
	}
	if !in.Draft {
		return PullRequest{}, fmt.Errorf("%w: non-draft create forbidden", ErrInvalidInput)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Lookup-before-create convergence: if an exact open draft already exists,
	// return it rather than inserting a second PR (concurrent worker safety).
	for _, pr := range f.prs {
		if pr.HeadRef == in.Head && pr.BaseRef == in.Base && pr.State == "open" &&
			pr.Draft && pr.Title == in.Title && pr.Body == in.Body {
			return pr, nil
		}
	}
	number := int(atomic.AddInt32(&f.nextPR, 1))
	pr := PullRequest{
		Number:  number,
		NodeID:  fmt.Sprintf("PR_kwfake_%d", number),
		HeadRef: in.Head,
		BaseRef: in.Base,
		Draft:   true,
		State:   "open",
		Title:   in.Title,
		Body:    in.Body,
	}
	f.prs = append(f.prs, pr)
	_ = owner
	_ = repo
	return pr, nil
}

// PRCount returns the number of stored PRs (test helper).
func (f *FakeAPI) PRCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prs)
}

func refKey(owner, repo, ref string) string {
	return owner + "/" + repo + "#" + ref
}

// RESTAPI is a thin GitHub REST client over HTTPDoer. Used for live dogfood
// later; unit tests prefer FakeAPI.
type RESTAPI struct {
	// HTTP is the transport (defaults to http.DefaultClient when nil).
	HTTP HTTPDoer
	// BaseURL defaults to https://api.github.com.
	BaseURL string
	// Token is the fine-grained PAT.
	Token string
}

// NewRESTAPI constructs a REST client. Token is required for live use.
func NewRESTAPI(httpDoer HTTPDoer, token string) *RESTAPI {
	if httpDoer == nil {
		httpDoer = http.DefaultClient
	}
	return &RESTAPI{HTTP: httpDoer, BaseURL: "https://api.github.com", Token: token}
}

// GetRef implements API.
func (r *RESTAPI) GetRef(ctx context.Context, owner, repo, ref string) (string, bool, error) {
	// GitHub expects the ref path without the "refs/" prefix for git/ref.
	trimmed := strings.TrimPrefix(ref, "refs/")
	url := fmt.Sprintf("%s/repos/%s/%s/git/ref/%s", r.BaseURL, owner, repo, trimmed)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	r.authorize(req)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("github: get ref status %d", resp.StatusCode)
	}
	var body struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false, err
	}
	return body.Object.SHA, body.Object.SHA != "", nil
}

// CreateRef implements API.
func (r *RESTAPI) CreateRef(ctx context.Context, owner, repo, ref, sha string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/git/refs", r.BaseURL, owner, repo)
	payload, _ := json.Marshal(map[string]string{"ref": ref, "sha": sha})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	r.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnprocessableEntity {
		// Already exists — reconcile via GetRef at the caller.
		return fmt.Errorf("%w: ref exists", ErrConflict)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github: create ref status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ListPullRequests implements API.
func (r *RESTAPI) ListPullRequests(ctx context.Context, owner, repo, head, base string) ([]PullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all&head=%s&base=%s", r.BaseURL, owner, repo, head, base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	r.authorize(req)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github: list prs status %d", resp.StatusCode)
	}
	var raw []struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Draft  bool   `json:"draft"`
		State  string `json:"state"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(raw))
	for _, item := range raw {
		out = append(out, PullRequest{
			Number:  item.Number,
			NodeID:  item.NodeID,
			HeadRef: item.Head.Ref,
			BaseRef: item.Base.Ref,
			HeadSHA: item.Head.SHA,
			BaseSHA: item.Base.SHA,
			Draft:   item.Draft,
			State:   item.State,
			Title:   item.Title,
			Body:    item.Body,
		})
	}
	return out, nil
}

// CreatePullRequest implements API.
func (r *RESTAPI) CreatePullRequest(ctx context.Context, owner, repo string, in CreatePRInput) (PullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", r.BaseURL, owner, repo)
	payload, _ := json.Marshal(map[string]any{
		"title": in.Title,
		"body":  in.Body,
		"head":  in.Head,
		"base":  in.Base,
		"draft": true, // always draft; never mergeable promotion here
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return PullRequest{}, err
	}
	r.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return PullRequest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return PullRequest{}, fmt.Errorf("github: create pr status %d: %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Draft  bool   `json:"draft"`
		State  string `json:"state"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{
		Number:  raw.Number,
		NodeID:  raw.NodeID,
		HeadRef: raw.Head.Ref,
		BaseRef: raw.Base.Ref,
		HeadSHA: raw.Head.SHA,
		BaseSHA: raw.Base.SHA,
		Draft:   raw.Draft,
		State:   raw.State,
		Title:   raw.Title,
		Body:    raw.Body,
	}, nil
}

func (r *RESTAPI) authorize(req *http.Request) {
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// RoundTripFunc adapts a function to http.RoundTripper for fake HTTP tests.
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// HTTPClientFromRoundTrip builds an *http.Client from a round-trip function.
func HTTPClientFromRoundTrip(fn RoundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}
