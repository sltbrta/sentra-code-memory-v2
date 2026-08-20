package authorityprocess

import (
	"context"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
)

// composeTracerAuthority wired the GitHub effect broker with tracerAllowPolicy:
// a Policy that returned Allowed: true unconditionally and echoed the request's
// own RevocationEpoch straight back, which made the broker's revocation
// comparison a tautology too. It was the only policy in front of branch
// publication and draft-PR creation, so with OUROBOROS_TRACER_LIVE_GITHUB=1 and
// a fine-grained PAT any authenticated local caller could write to the real
// repository with no policy check and no way to revoke.

// TestTracerCompositionRequiresARealPolicyBroker pins the dependency. The
// previous signature took `_ dependencies` and ignored the broker entirely, so
// there was nothing a test could observe; requiring it is what makes the
// allow-all stand-in unreachable.
func TestTracerCompositionRequiresARealPolicyBroker(t *testing.T) {
	config := &localbootstrap.Config{}
	if _, _, err := composeTracerAuthority(context.Background(), config, dependencies{}); err == nil {
		t.Fatal("composing the tracer authority without a policy broker must fail")
	}
}
