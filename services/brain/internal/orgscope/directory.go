package orgscope

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrDenied is the single non-disclosing denial for every refused access.
var ErrDenied = errors.New("orgscope: not_found_or_denied")

// ErrRejected reports a typed, non-disclosing validation failure.
var ErrRejected = errors.New("orgscope: rejected")

// Receipt kinds emitted by lifecycle and policy mutations.
const (
	ReceiptUserProvision   = "user.provision"
	ReceiptUserDeprovision = "user.deprovision"
	ReceiptGroupCreate     = "group.create"
	ReceiptGroupDelete     = "group.delete"
	ReceiptMemberAdd       = "group.member.add"
	ReceiptMemberRemove    = "group.member.remove"
	ReceiptGrantCreate     = "grant.create"
	ReceiptGrantRevoke     = "grant.revoke"
	ReceiptDenyOverlay     = "deny.overlay"
	ReceiptErasure         = "erasure"
)

// Receipt is one append-only policy/lifecycle revocation record. Receipts
// carry the tenant and policy epoch after the mutation so projections can
// observe ordering; they are not cryptographic attestations.
type Receipt struct {
	TenantID string    `json:"tenant_id"`
	Seq      uint64    `json:"seq"`
	Kind     string    `json:"kind"`
	Subject  string    `json:"subject"`
	Object   string    `json:"object,omitempty"`
	Epoch    uint64    `json:"epoch"`
	At       time.Time `json:"at"`
}

// Directory is the SCIM-shaped user/group lifecycle source of truth for one
// tenant. Every mutation bumps the policy epoch and appends a receipt.
type Directory struct {
	mu       sync.Mutex
	tenantID string
	epoch    uint64
	seq      uint64
	users    map[string]bool            // user id -> active
	versions map[string]uint64          // user id -> lifecycle incarnation
	groups   map[string]map[string]bool // group id -> member set
	groupVer map[string]uint64          // group id -> lifecycle incarnation
	receipts []Receipt
	now      func() time.Time
}

// NewDirectory returns an empty directory for one tenant. A nil clock uses
// time.Now; tests inject a deterministic clock.
func NewDirectory(tenantID string, clock func() time.Time) (*Directory, error) {
	if !validID(tenantID) {
		return nil, ErrRejected
	}
	if clock == nil {
		clock = time.Now
	}
	return &Directory{
		tenantID: tenantID,
		users:    make(map[string]bool),
		versions: make(map[string]uint64),
		groups:   make(map[string]map[string]bool),
		groupVer: make(map[string]uint64),
		now:      clock,
	}, nil
}

// validID rejects empty ids and separator/traversal characters used in
// composite keys and paths.
func validID(id string) bool {
	if id == "" || id != strings.TrimSpace(id) {
		return false
	}
	if strings.ContainsAny(id, "|/\\") || strings.Contains(id, "..") {
		return false
	}
	return true
}

// TenantID returns the directory tenant.
func (d *Directory) TenantID() string { return d.tenantID }

// Epoch returns the current policy epoch.
func (d *Directory) Epoch() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.epoch
}

// clock returns the injected time source.
func (d *Directory) clock() time.Time { return d.now() }

// emit appends one receipt under the lock, bumping seq and epoch.
func (d *Directory) emit(kind, subject, object string) Receipt {
	d.seq++
	d.epoch++
	r := Receipt{
		TenantID: d.tenantID, Seq: d.seq, Kind: kind, Subject: subject, Object: object,
		Epoch: d.epoch, At: d.now().UTC(),
	}
	d.receipts = append(d.receipts, r)
	return r
}

// receiptFor appends a receipt for a policy mutation owned by a peer
// component (authority grants/denies, store erasure).
func (d *Directory) receiptFor(kind, subject, object string) Receipt {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.emit(kind, subject, object)
}

// Provision activates a user (idempotent for an already-active user).
func (d *Directory) Provision(userID string) (Receipt, error) {
	if !validID(userID) {
		return Receipt{}, ErrRejected
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.users[userID] {
		d.versions[userID]++
	}
	d.users[userID] = true
	return d.emit(ReceiptUserProvision, userID, ""), nil
}

// Deprovision offboards a user: marks inactive and removes every group
// membership. The receipt records the offboarding revocation.
func (d *Directory) Deprovision(userID string) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.users[userID] {
		return Receipt{}, ErrDenied
	}
	d.users[userID] = false
	d.versions[userID]++
	for _, members := range d.groups {
		delete(members, userID)
	}
	return d.emit(ReceiptUserDeprovision, userID, ""), nil
}

// EnsureGroup creates a group if missing.
func (d *Directory) EnsureGroup(groupID string) (Receipt, error) {
	if !validID(groupID) {
		return Receipt{}, ErrRejected
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.groups[groupID]; !ok {
		d.groupVer[groupID]++
		d.groups[groupID] = make(map[string]bool)
	}
	return d.emit(ReceiptGroupCreate, groupID, ""), nil
}

// DeleteGroup removes a group and all memberships. Recreating the same
// external id starts a new incarnation, so old group grants stay revoked.
func (d *Directory) DeleteGroup(groupID string) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.groups[groupID]; !ok {
		return Receipt{}, ErrDenied
	}
	delete(d.groups, groupID)
	d.groupVer[groupID]++
	return d.emit(ReceiptGroupDelete, groupID, ""), nil
}

// AddMember joins an active user to an existing group.
func (d *Directory) AddMember(groupID, userID string) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	members, ok := d.groups[groupID]
	if !ok || !d.users[userID] {
		return Receipt{}, ErrDenied
	}
	members[userID] = true
	return d.emit(ReceiptMemberAdd, userID, groupID), nil
}

// RemoveMember revokes a group membership (role change).
func (d *Directory) RemoveMember(groupID, userID string) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	members, ok := d.groups[groupID]
	if !ok || !members[userID] {
		return Receipt{}, ErrDenied
	}
	delete(members, userID)
	return d.emit(ReceiptMemberRemove, userID, groupID), nil
}

// IsActive reports whether a user is provisioned and not offboarded.
func (d *Directory) IsActive(userID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.users[userID]
}

// Incarnation returns the active user's lifecycle incarnation. A grant bound
// to an older incarnation stays revoked if the same external id is later
// provisioned again.
func (d *Directory) Incarnation(userID string) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.versions[userID], d.users[userID]
}

// HasGroup reports whether a group exists.
func (d *Directory) HasGroup(groupID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.groups[groupID]
	return ok
}

// GroupIncarnation returns an existing group's lifecycle incarnation.
func (d *Directory) GroupIncarnation(groupID string) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, exists := d.groups[groupID]
	return d.groupVer[groupID], exists
}

// MemberGroups returns the groups an active user belongs to.
func (d *Directory) MemberGroups(userID string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.users[userID] {
		return nil
	}
	var out []string
	for gid, members := range d.groups {
		if members[userID] {
			out = append(out, gid)
		}
	}
	return out
}

// Receipts returns a copy of the append-only receipt log.
func (d *Directory) Receipts() []Receipt {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Receipt, len(d.receipts))
	copy(out, d.receipts)
	return out
}
