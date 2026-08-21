package github_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/github"
)

// A-010. Both stale-base checks read `if ok && baseSHA != approved`, so an
// absent base ref -- deleted, renamed, or misspelled -- skipped the
// approved-base binding entirely and publication proceeded. The check exists
// to guarantee "the provider base still equals the approved base"; the one
// case where the provider base cannot equal anything was the case it let
// through.
//
// The fix was verified by reading. Nothing exercised a missing base ref, so
// reverting it left the suite green.

func staleBaseDenial(t *testing.T, err error) {
	t.Helper()
	var denial *github.Denial
	if !errors.As(err, &denial) {
		t.Fatalf("want a stale-base denial, got %v", err)
	}
	if denial.Reason != github.ReasonStaleBase {
		t.Fatalf("want reason %q, got %q", github.ReasonStaleBase, denial.Reason)
	}
}

// TestPublishDeniesWhenTheBaseRefIsAbsent covers ensureBranch: the base branch
// was deleted or renamed between approval and publication.
func TestPublishDeniesWhenTheBaseRefIsAbsent(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	// Deliberately no SeedRef for refs/heads/main: the approved base is gone.
	broker := newBroker(t, api, allowPolicy{})

	_, err := broker.Publish(context.Background(), baseRequest())
	if err == nil {
		t.Fatal("publication proceeded against a base ref that does not exist: " +
			"the approved-base binding was skipped rather than enforced")
	}
	staleBaseDenial(t, err)
	if api.CreateCalls != 0 {
		t.Fatalf("a pull request was created against a missing base (%d create calls)", api.CreateCalls)
	}
}

// vanishingBaseAPI answers the base ref once and reports it absent from then
// on: the base branch is deleted after the branch is published but before the
// draft PR is opened. It reaches the second stale-base check, which had the
// same defect.
type vanishingBaseAPI struct {
	*github.FakeAPI
	baseLookups int32
}

func (a *vanishingBaseAPI) GetRef(ctx context.Context, owner, repo, ref string) (string, bool, error) {
	if strings.HasSuffix(ref, "/main") {
		if atomic.AddInt32(&a.baseLookups, 1) > 1 {
			return "", false, nil
		}
	}
	return a.FakeAPI.GetRef(ctx, owner, repo, ref)
}

func TestPublishDeniesWhenTheBaseRefVanishesBeforeThePR(t *testing.T) {
	t.Parallel()
	fake := github.NewFakeAPI()
	fake.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	api := &vanishingBaseAPI{FakeAPI: fake}
	broker := newBroker(t, api, allowPolicy{})

	_, err := broker.Publish(context.Background(), baseRequest())
	if err == nil {
		t.Fatal("a draft PR was opened after the approved base disappeared")
	}
	staleBaseDenial(t, err)
	if fake.PRCount() != 0 {
		t.Fatalf("pr count = %d, want 0", fake.PRCount())
	}
}

// TestPublishStillDeniesAMovedBase keeps the original condition covered: a
// base ref that is present but has moved off the approved OID.
func TestPublishStillDeniesAMovedBase(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main",
		"1111111111111111111111111111111111111111")
	broker := newBroker(t, api, allowPolicy{})

	_, err := broker.Publish(context.Background(), baseRequest())
	if err == nil {
		t.Fatal("publication proceeded against a base that moved off the approved OID")
	}
	staleBaseDenial(t, err)
}
