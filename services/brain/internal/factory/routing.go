package factory

import (
	"context"
	"regexp"
)

var (
	modelIdentityPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
	rationaleCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// StaticRouter is the deterministic certified-profile Router used in tests and
// by the bounded v1 composition: every leaf of every run routes to the same
// pinned certified profile, so route decisions replay exactly.
type StaticRouter struct {
	// ProfileDigestHex pins the certified runner/model/profile selection policy.
	ProfileDigestHex string
	// ModelIdentity names the selected provider-neutral model identity.
	ModelIdentity string
	// RationaleCode is a stable non-sensitive routing rationale under policy.
	RationaleCode string
}

// NewStaticRouter validates the pinned routing facts.
func NewStaticRouter(profileDigestHex, modelIdentity, rationaleCode string) (StaticRouter, error) {
	router := StaticRouter{
		ProfileDigestHex: profileDigestHex,
		ModelIdentity:    modelIdentity,
		RationaleCode:    rationaleCode,
	}
	if !isHexDigest(router.ProfileDigestHex) || !modelIdentityPattern.MatchString(router.ModelIdentity) ||
		len(router.RationaleCode) == 0 || len(router.RationaleCode) > 64 ||
		!rationaleCodePattern.MatchString(router.RationaleCode) {
		return StaticRouter{}, ErrInvalidInput
	}
	return router, nil
}

// Route returns the pinned decision for any well-formed leaf request.
func (r StaticRouter) Route(ctx context.Context, request RouteRequest) (RouteDecision, error) {
	if ctx == nil || request.RunID == "" || request.NodeID == "" || !isHexDigest(request.GoalDigestHex) ||
		len(request.OwnedPaths) == 0 {
		return RouteDecision{}, ErrInvalidInput
	}
	return RouteDecision{
		ProfileDigestHex: r.ProfileDigestHex,
		ModelIdentity:    r.ModelIdentity,
		RationaleCode:    r.RationaleCode,
	}, nil
}
