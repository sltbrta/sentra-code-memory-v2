// Package authz evaluates the Stage 02 OpenFGA-compatible local relationship subset.
// Tenant membership never implies brain or evidence access; current revocation wins.
package authz

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var ErrMalformedTuple = errors.New("authz: malformed tuple")

// Tuple is one OpenFGA-shaped relationship fact.
type Tuple struct {
	Object   string
	Relation string
	User     string
}

// Evaluator provides the pinned local model semantics with a current deny overlay.
// It is intentionally not a substitute for full OpenFGA server/model conformance.
type Evaluator struct {
	mu     sync.RWMutex
	tuples map[Tuple]struct{}
	epochs map[string]uint64
}

// NewEvaluator returns a default-deny empty relationship evaluator.
func NewEvaluator() *Evaluator {
	return &Evaluator{tuples: make(map[Tuple]struct{}), epochs: make(map[string]uint64)}
}

// Write adds a fully qualified relationship tuple.
func (e *Evaluator) Write(tuple Tuple) error {
	if !validTuple(tuple) {
		return ErrMalformedTuple
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tuples[tuple] = struct{}{}
	return nil
}

// Delete removes an existing relationship and advances its derived owning tenant epoch.
// Tenant selection happens under the same lock as deletion; callers cannot name it.
func (e *Evaluator) Delete(tuple Tuple) (string, uint64, error) {
	if !validTuple(tuple) {
		return "", 0, ErrMalformedTuple
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.tuples[tuple]; !exists {
		return "", 0, ErrMalformedTuple
	}
	tenant, ok := e.owningTenant(tuple.Object)
	if !ok {
		return "", 0, ErrMalformedTuple
	}
	delete(e.tuples, tuple)
	e.epochs[tenant]++
	return tenant, e.epochs[tenant], nil
}

// SetEpoch advances the current tenant deny epoch; it never moves backward.
func (e *Evaluator) SetEpoch(tenant string, epoch uint64) error {
	if tenant == "" {
		return ErrMalformedTuple
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if epoch < e.epochs[tenant] {
		return ErrMalformedTuple
	}
	e.epochs[tenant] = epoch
	return nil
}

// Epoch returns the current tenant deny epoch without evaluating a resource.
func (e *Evaluator) Epoch(tenant string) (uint64, error) {
	if e == nil || tenant == "" {
		return 0, ErrMalformedTuple
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.epochs[tenant], nil
}

// Check implements contracts.PolicyCheck for evidence read/admit/delete actions.
// The request epoch must equal current state; missing and unknown facts deny.
func (e *Evaluator) Check(_ context.Context, identity contracts.MappedIdentityFact, request contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	decision := contracts.PolicyDecision{Receipt: deniedReceipt(), Allowed: false}
	if identity.Principal.Namespace != "principal" || identity.Principal.Value == "" ||
		identity.Tenant.Namespace != "tenant" || identity.Tenant.Value == "" ||
		request.Resource.Namespace != "evidence" || request.Resource.Value == "" {
		return decision, nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	current := e.epochs[identity.Tenant.Value]
	decision.RevocationEpoch = current
	if request.RevocationEpoch != current {
		return decision, nil
	}
	evidence := "evidence:" + request.Resource.Value
	user := "user:" + identity.Principal.Value
	allowed := false
	if request.Action != "artifact.read" && request.Action != "artifact.admit" && request.Action != "artifact.delete" {
		return decision, nil
	}
	for _, brain := range e.relatedObjects(evidence, "brain", "brain:") {
		if !e.has(brain, "tenant", "tenant:"+identity.Tenant.Value) {
			continue
		}
		if request.Action == "artifact.read" {
			allowed = e.has(brain, "owner", user) || e.has(brain, "viewer", user)
		} else {
			allowed = e.has(brain, "owner", user)
		}
		if allowed {
			break
		}
	}
	if allowed {
		decision.Allowed = true
		decision.Receipt.Status = "completed"
		decision.Receipt.ReasonCode = "allowed"
	}
	return decision, nil
}

// CheckSource evaluates one current source, query, or Stage 05 factory action
// against the exact tenant-scoped brain relationship. Source mutation requires
// owner; status, search, and the Stage 04 query funnel checkpoints (query,
// hydrate, emit) may also use viewer; factory admission, cancellation, and the
// sealed leaf file effects require owner. It deliberately returns the same
// denial for an absent brain, a different tenant, and a removed relationship.
func (e *Evaluator) CheckSource(_ context.Context, identity contracts.MappedIdentityFact, action string, brain contracts.Identifier) (contracts.PolicyDecision, error) {
	decision := contracts.PolicyDecision{Receipt: deniedReceipt(), Allowed: false}
	if e == nil || identity.Principal.Namespace != "principal" || identity.Principal.Value == "" ||
		identity.Tenant.Namespace != "tenant" || identity.Tenant.Value == "" ||
		brain.Namespace != "brain" || brain.Value == "" {
		return decision, nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	decision.RevocationEpoch = e.epochs[identity.Tenant.Value]
	brainObject := "brain:" + brain.Value
	if !e.has(brainObject, "tenant", "tenant:"+identity.Tenant.Value) {
		return decision, nil
	}
	user := "user:" + identity.Principal.Value
	owner := e.has(brainObject, "owner", user)
	allowed := false
	switch action {
	case "source.add", "source.reconcile", "source.revoke":
		allowed = owner
	case "factory.admit", "factory.cancel", "file.read", "file.write":
		// The Stage 05 factory funnel: admission, cancellation, and the sealed
		// leaf file effects all require current owner authority on the brain.
		allowed = owner
	case "source.status", "source.search", "query", "hydrate", "emit":
		allowed = owner || e.has(brainObject, "viewer", user)
	default:
		return decision, nil
	}
	if allowed {
		decision.Allowed = true
		decision.Receipt.Status = "completed"
		decision.Receipt.ReasonCode = "allowed"
	}
	return decision, nil
}

func (e *Evaluator) relatedObjects(object, relation, prefix string) []string {
	objects := make([]string, 0)
	for tuple := range e.tuples {
		if tuple.Object == object && tuple.Relation == relation && len(tuple.User) > len(prefix) && tuple.User[:len(prefix)] == prefix {
			objects = append(objects, tuple.User)
		}
	}
	sort.Strings(objects)
	return objects
}

func (e *Evaluator) has(object, relation, user string) bool {
	_, ok := e.tuples[Tuple{Object: object, Relation: relation, User: user}]
	return ok
}

func (e *Evaluator) owningTenant(object string) (string, bool) {
	entityType, entityID, ok := splitEntity(object)
	if !ok {
		return "", false
	}
	if entityType == "tenant" {
		return entityID, true
	}
	brains := []string{object}
	if entityType == "evidence" {
		brains = e.relatedObjects(object, "brain", "brain:")
	} else if entityType != "brain" {
		return "", false
	}
	tenants := make(map[string]struct{})
	for _, brain := range brains {
		for _, tenant := range e.relatedObjects(brain, "tenant", "tenant:") {
			_, tenantID, valid := splitEntity(tenant)
			if valid {
				tenants[tenantID] = struct{}{}
			}
		}
	}
	if len(tenants) != 1 {
		return "", false
	}
	for tenant := range tenants {
		return tenant, true
	}
	return "", false
}

func validTuple(tuple Tuple) bool {
	return tuple.Object != "" && tuple.Relation != "" && tuple.User != "" &&
		containsColon(tuple.Object) && containsColon(tuple.User)
}

func containsColon(value string) bool {
	_, _, ok := splitEntity(value)
	return ok
}

func splitEntity(value string) (string, string, bool) {
	entityType, entityID, ok := strings.Cut(value, ":")
	return entityType, entityID, ok && entityType != "" && entityID != ""
}

func deniedReceipt() contracts.Receipt {
	return contracts.Receipt{
		Status:      "rejected",
		ReasonCode:  "not_found_or_denied",
		OperationID: contracts.Identifier{Namespace: "authorization", Value: "current-check"},
	}
}

// ParseTuple parses the checked-in `object#relation@user` fixture vocabulary.
func ParseTuple(value string) (Tuple, error) {
	hash := strings.IndexByte(value, '#')
	at := strings.IndexByte(value, '@')
	if hash <= 0 || at <= hash+1 || at == len(value)-1 {
		return Tuple{}, ErrMalformedTuple
	}
	tuple := Tuple{Object: value[:hash], Relation: value[hash+1 : at], User: value[at+1:]}
	if !validTuple(tuple) {
		return Tuple{}, ErrMalformedTuple
	}
	return tuple, nil
}

// hasTuple reports whether the exact relationship is present.
func (e *Evaluator) hasTuple(tuple Tuple) bool {
	if e == nil {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.has(tuple.Object, tuple.Relation, tuple.User)
}

// peekOwningTenant returns the unique derived tenant for object without mutation.
func (e *Evaluator) peekOwningTenant(object string) (string, bool) {
	if e == nil {
		return "", false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.owningTenant(object)
}

// evidenceInTenant reports whether evidence is linked to a brain in tenant.
func (e *Evaluator) evidenceInTenant(evidenceObject, tenant string) bool {
	if e == nil || tenant == "" {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, brain := range e.relatedObjects(evidenceObject, "brain", "brain:") {
		if e.has(brain, "tenant", "tenant:"+tenant) {
			return true
		}
	}
	return false
}

// brainInTenant reports whether brain is bound to tenant.
func (e *Evaluator) brainInTenant(brainObject, tenant string) bool {
	if e == nil || tenant == "" {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.has(brainObject, "tenant", "tenant:"+tenant)
}
