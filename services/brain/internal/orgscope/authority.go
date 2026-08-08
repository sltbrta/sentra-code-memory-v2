package orgscope

import (
	"sync"
	"time"
)

// ScopeKind is the memory visibility tier.
type ScopeKind string

const (
	// ScopeIndividual is one user's private memory.
	ScopeIndividual ScopeKind = "individual"
	// ScopeTeam is one group's shared memory.
	ScopeTeam ScopeKind = "team"
	// ScopeCompany is tenant-wide shared memory.
	ScopeCompany ScopeKind = "company"
)

// Scope identifies one memory scope inside a tenant.
type Scope struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id,omitempty"` // user id (individual) or group id (team); empty for company
}

// Key returns the canonical scope key.
func (s Scope) Key() string {
	if s.Kind == ScopeCompany {
		return string(ScopeCompany)
	}
	return string(s.Kind) + ":" + s.ID
}

// valid rejects malformed scopes fail-closed.
func (s Scope) valid() bool {
	switch s.Kind {
	case ScopeIndividual, ScopeTeam:
		return validID(s.ID)
	case ScopeCompany:
		return s.ID == ""
	default:
		return false
	}
}

// Principal is one authenticated caller.
type Principal struct {
	UserID   string
	TenantID string
}

// Grant allows a subject ("user:<id>" or "group:<id>") to read a scope.
// A zero ExpiresAt is durable; otherwise the grant dies at that instant.
// A non-empty DelegatedBy ties the grant's life to the delegator staying
// active: offboarding the delegator revokes the grant fail-closed.
type Grant struct {
	Subject     string    `json:"subject"`
	Scope       Scope     `json:"scope"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	DelegatedBy string    `json:"delegated_by,omitempty"`
}

type grantRecord struct {
	Grant
	subjectIncarnation   uint64
	delegatorIncarnation uint64
}

// Authority is the default-deny scope evaluator with deny overlays. Every
// path that is not an explicit live allow resolves to ErrDenied.
type Authority struct {
	mu     sync.Mutex
	dir    *Directory
	grants map[string]map[string]grantRecord // subject -> scope key -> grant
	denies map[string]bool                   // user id + "|" + scope key
}

// NewAuthority wires an evaluator over one tenant directory.
func NewAuthority(dir *Directory) *Authority {
	return &Authority{
		dir:    dir,
		grants: make(map[string]map[string]grantRecord),
		denies: make(map[string]bool),
	}
}

// Directory returns the backing lifecycle directory.
func (a *Authority) Directory() *Directory { return a.dir }

// Grant records one allow edge and emits a receipt. User subjects must be
// active; group subjects must exist; delegators must be active.
func (a *Authority) Grant(g Grant) (Receipt, error) {
	if !g.Scope.valid() {
		return Receipt{}, ErrRejected
	}
	kind, id, ok := splitSubject(g.Subject)
	if !ok {
		return Receipt{}, ErrRejected
	}
	var subjectIncarnation uint64
	switch kind {
	case "user":
		var active bool
		subjectIncarnation, active = a.dir.Incarnation(id)
		if !active {
			return Receipt{}, ErrDenied
		}
	case "group":
		var exists bool
		subjectIncarnation, exists = a.dir.GroupIncarnation(id)
		if !exists {
			return Receipt{}, ErrDenied
		}
	}
	var delegatorIncarnation uint64
	if g.DelegatedBy != "" {
		var active bool
		delegatorIncarnation, active = a.dir.Incarnation(g.DelegatedBy)
		if !active {
			return Receipt{}, ErrDenied
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grants[g.Subject] == nil {
		a.grants[g.Subject] = make(map[string]grantRecord)
	}
	a.grants[g.Subject][g.Scope.Key()] = grantRecord{
		Grant: g, subjectIncarnation: subjectIncarnation,
		delegatorIncarnation: delegatorIncarnation,
	}
	return a.dir.receiptFor(ReceiptGrantCreate, g.Subject, g.Scope.Key()), nil
}

// Revoke removes an allow edge and emits a revocation receipt.
func (a *Authority) Revoke(subject string, scope Scope) (Receipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	byScope, ok := a.grants[subject]
	if ok {
		_, ok = byScope[scope.Key()]
		delete(byScope, scope.Key())
	}
	if !ok {
		return Receipt{}, ErrDenied
	}
	return a.dir.receiptFor(ReceiptGrantRevoke, subject, scope.Key()), nil
}

// Deny records a deny overlay for a user on a scope. Deny beats every allow,
// including the implicit own-individual allow.
func (a *Authority) Deny(userID string, scope Scope) (Receipt, error) {
	if !validID(userID) || !scope.valid() {
		return Receipt{}, ErrRejected
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.denies[userID+"|"+scope.Key()] = true
	return a.dir.receiptFor(ReceiptDenyOverlay, userID, scope.Key()), nil
}

// maxAuthPasses bounds re-evaluation when policy mutates concurrently with a
// read. If the policy epoch will not hold still, evaluation fails closed.
const maxAuthPasses = 4

// Resolve returns nil only for an explicit live allow; every other outcome is
// ErrDenied (default deny, non-disclosing). The decision is only returned if
// the policy epoch is unchanged across the evaluation, so a concurrent
// offboarding, revocation, or deny overlay always wins: the evaluation reruns
// against the mutated policy, and an unsettled policy denies fail-closed.
func (a *Authority) Resolve(p Principal, scope Scope) error {
	for pass := 0; pass < maxAuthPasses; pass++ {
		epoch := a.dir.Epoch()
		err := a.resolveOnce(p, scope)
		if a.dir.Epoch() == epoch {
			return err
		}
	}
	return ErrDenied
}

// resolveOnce evaluates one allow/deny pass against current state.
func (a *Authority) resolveOnce(p Principal, scope Scope) error {
	if p.TenantID != a.dir.TenantID() || !scope.valid() {
		return ErrDenied
	}
	if !a.dir.IsActive(p.UserID) {
		return ErrDenied
	}
	now := a.dir.clock()
	groups := a.dir.MemberGroups(p.UserID)
	principalIncarnation, active := a.dir.Incarnation(p.UserID)
	if !active {
		return ErrDenied
	}
	a.mu.Lock()
	if a.denies[p.UserID+"|"+scope.Key()] {
		a.mu.Unlock()
		return ErrDenied
	}
	// Implicit: an active, non-denied user reads their own individual scope.
	if scope.Kind == ScopeIndividual && scope.ID == p.UserID {
		a.mu.Unlock()
		return nil
	}
	candidates := []grantRecord{a.grants["user:"+p.UserID][scope.Key()]}
	for _, gid := range groups {
		candidates = append(candidates, a.grants["group:"+gid][scope.Key()])
	}
	a.mu.Unlock()
	for _, grant := range candidates {
		if a.liveGrant(grant, principalIncarnation, now) {
			return nil
		}
	}
	return ErrDenied
}

// liveGrant checks one copied allow edge without holding the authority lock.
func (a *Authority) liveGrant(g grantRecord, principalIncarnation uint64, now time.Time) bool {
	if g.Subject == "" {
		return false
	}
	if !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt) {
		return false
	}
	kind, id, _ := splitSubject(g.Subject)
	if kind == "user" && g.subjectIncarnation != principalIncarnation {
		return false
	}
	if kind == "group" {
		incarnation, exists := a.dir.GroupIncarnation(id)
		if !exists || incarnation != g.subjectIncarnation {
			return false
		}
	}
	if g.DelegatedBy != "" {
		incarnation, active := a.dir.Incarnation(g.DelegatedBy)
		if !active || incarnation != g.delegatorIncarnation {
			return false
		}
	}
	return true
}

// Epoch exposes the policy epoch for cache/projection observability.
func (a *Authority) Epoch() uint64 { return a.dir.Epoch() }

func splitSubject(subject string) (kind, id string, ok bool) {
	for _, prefix := range []string{"user:", "group:"} {
		if len(subject) > len(prefix) && subject[:len(prefix)] == prefix {
			id = subject[len(prefix):]
			if validID(id) {
				return prefix[:len(prefix)-1], id, true
			}
		}
	}
	return "", "", false
}
