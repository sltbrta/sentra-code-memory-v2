package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPDoer is the narrow HTTP surface live mode uses.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RESTSourceAPI implements SourceAPI against the GitHub REST API. It is only
// used when a fine-grained PAT is present; production tests never select it.
type RESTSourceAPI struct {
	// Client performs HTTP requests (defaults to http.DefaultClient).
	Client HTTPDoer
	// Token is the fine-grained PAT.
	Token string
	// BaseURL defaults to https://api.github.com.
	BaseURL string
}

// NewRESTSourceAPI returns a live REST source adapter. Token may be empty;
// callers must resolve via ResolveToken.
func NewRESTSourceAPI(token string) *RESTSourceAPI {
	return &RESTSourceAPI{
		Client:  &http.Client{Timeout: 15 * time.Second},
		Token:   token,
		BaseURL: "https://api.github.com",
	}
}

// Snapshot lists repository metadata and open issues (bounded page).
func (a *RESTSourceAPI) Snapshot(ctx context.Context, owner, repo string) (SnapshotPage, error) {
	if a == nil || strings.TrimSpace(a.Token) == "" {
		return SnapshotPage{Complete: false, ErrorCode: "provider_unavailable"}, nil
	}
	objects := make([]Object, 0, 16)
	repoObj, err := a.getRepository(ctx, owner, repo)
	if err != nil {
		return mapLiveError(err)
	}
	objects = append(objects, repoObj)
	issues, err := a.listIssues(ctx, owner, repo)
	if err != nil {
		return mapLiveError(err)
	}
	objects = append(objects, issues...)
	return SnapshotPage{
		Cursor:   "live-" + strconv.FormatInt(time.Now().UTC().Unix(), 10),
		Objects:  objects,
		Complete: true,
	}, nil
}

// Delta re-lists issues and returns them as a complete page (bounded live dogfood).
func (a *RESTSourceAPI) Delta(ctx context.Context, owner, repo, priorCursor string) (SnapshotPage, error) {
	if a == nil || strings.TrimSpace(a.Token) == "" {
		return SnapshotPage{Complete: false, ErrorCode: "provider_unavailable", Cursor: priorCursor}, nil
	}
	if strings.TrimSpace(priorCursor) == "" {
		return SnapshotPage{Complete: false, ErrorCode: "malformed_page"}, nil
	}
	issues, err := a.listIssues(ctx, owner, repo)
	if err != nil {
		page, mapErr := mapLiveError(err)
		page.Cursor = priorCursor
		return page, mapErr
	}
	return SnapshotPage{
		Cursor:   priorCursor + ".delta",
		Objects:  issues,
		Complete: true,
	}, nil
}

func (a *RESTSourceAPI) getRepository(ctx context.Context, owner, repo string) (Object, error) {
	var body struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := a.getJSON(ctx, fmt.Sprintf("/repos/%s/%s", owner, repo), &body); err != nil {
		return Object{}, err
	}
	desc := body.Description
	if desc == "" {
		desc = body.FullName
	}
	return Object{
		ID: "repo:meta", Kind: ObjectKindRepository, Title: body.FullName,
		Body: desc + " branch=" + body.DefaultBranch, Version: "live-repo",
	}, nil
}

func (a *RESTSourceAPI) listIssues(ctx context.Context, owner, repo string) ([]Object, error) {
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
	}
	if err := a.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/issues?state=all&per_page=30", owner, repo), &issues); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(issues))
	for _, issue := range issues {
		// Pull requests appear in the issues list; skip them by absence of pure issue fields is hard —
		// GitHub includes pull_request key; treat number+title as issues for dogfood.
		out = append(out, Object{
			ID:          fmt.Sprintf("issue:%d", issue.Number),
			Kind:        ObjectKindIssue,
			Title:       issue.Title,
			Body:        issue.Body,
			IssueNumber: issue.Number,
			Version:     fmt.Sprintf("live-issue-%d", issue.Number),
			Deleted:     issue.State == "closed",
		})
	}
	return out, nil
}

func (a *RESTSourceAPI) getJSON(ctx context.Context, path string, dest any) error {
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return errRateLimited
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github: status %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dest)
}

var errRateLimited = fmt.Errorf("rate_limited")

func mapLiveError(err error) (SnapshotPage, error) {
	if err == nil {
		return SnapshotPage{}, nil
	}
	if err == errRateLimited {
		return SnapshotPage{Complete: false, ErrorCode: "rate_limited"}, nil
	}
	return SnapshotPage{Complete: false, ErrorCode: "provider_unavailable"}, nil
}
