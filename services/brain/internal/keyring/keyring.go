// Package keyring resolves tenant-scoped current and historical encryption roots.
// Production material lives in macOS Keychain; the in-memory implementation is
// deliberately isolated for deterministic tests and local conformance fixtures.
package keyring

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	// RootKeyBytes is the required AES-256 root-key length.
	RootKeyBytes = 32
)

var (
	// ErrInvalidMaterial reports malformed tenant, reference, or key material.
	ErrInvalidMaterial = errors.New("keyring: invalid material")
	// ErrNotFound reports a tenant-scoped epoch that does not exist.
	ErrNotFound = errors.New("keyring: epoch not found")
	// ErrUnreadable reports a known epoch whose material cannot be recovered.
	ErrUnreadable = errors.New("keyring: material unreadable")
	// ErrKeyConflict reports an immutable key reference already bound to different material.
	ErrKeyConflict = errors.New("keyring: immutable key conflict")
)

// State records whether an epoch may write, read historically, migrate legacy
// data, or must fail closed. It is metadata and never includes key bytes.
type State string

const (
	// Current is the sole epoch used to wrap newly generated data keys.
	Current State = "current"
	// Historical remains readable but cannot encrypt new data.
	Historical State = "historical"
	// Legacy may only be used by an explicit migration path.
	Legacy State = "legacy"
	// Unreadable forces callers to quarantine affected artifacts.
	Unreadable State = "unreadable"
)

// Material combines an opaque persisted reference with transient root bytes.
// Callers must not serialize, log, or retain RootKey after the cryptographic operation.
type Material struct {
	Reference contracts.KeyReference
	RootKey   []byte
}

// Resolver returns copied, tenant-scoped key material or a typed fail-closed error.
type Resolver interface {
	// Current returns the sole current root. It returns ErrNotFound for no current epoch.
	Current(context.Context, contracts.Identifier) (Material, error)
	// Resolve returns current, historical, or legacy material for an exact epoch.
	Resolve(context.Context, contracts.Identifier, uint64) (Material, error)
}

type entry struct {
	material Material
	state    State
}

// Memory is a concurrency-safe deterministic resolver for tests and isolated fixtures.
// It is not a production persistence fallback and copies key bytes at every boundary.
type Memory struct {
	mu      sync.RWMutex
	entries map[string]map[uint64]entry
}

// NewMemory constructs an empty isolated resolver.
func NewMemory() *Memory {
	return &Memory{entries: make(map[string]map[uint64]entry)}
}

// Add records one test epoch. Invalid material is retained as unreadable so later
// resolution fails closed instead of silently substituting another tenant or epoch.
func (m *Memory) Add(tenant contracts.Identifier, material Material, state State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[tenant.Value] == nil {
		m.entries[tenant.Value] = make(map[uint64]entry)
	}
	material.RootKey = append([]byte(nil), material.RootKey...)
	m.entries[tenant.Value][material.Reference.Epoch] = entry{material: material, state: state}
}

// Current returns copied material for the tenant's unique current epoch.
func (m *Memory) Current(_ context.Context, tenant contracts.Identifier) (Material, error) {
	if err := validateTenant(tenant); err != nil {
		return Material{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var found *entry
	for _, candidate := range m.entries[tenant.Value] {
		if candidate.state == Current {
			if found != nil {
				return Material{}, fmt.Errorf("%w: multiple current epochs", ErrInvalidMaterial)
			}
			copy := candidate
			found = &copy
		}
	}
	if found == nil {
		return Material{}, ErrNotFound
	}
	return copyMaterial(found.material)
}

// Resolve returns copied material for one exact tenant epoch.
func (m *Memory) Resolve(_ context.Context, tenant contracts.Identifier, epoch uint64) (Material, error) {
	if err := validateTenant(tenant); err != nil {
		return Material{}, err
	}
	m.mu.RLock()
	candidate, ok := m.entries[tenant.Value][epoch]
	m.mu.RUnlock()
	if !ok {
		return Material{}, ErrNotFound
	}
	if candidate.state == Unreadable {
		return Material{}, ErrUnreadable
	}
	return copyMaterial(candidate.material)
}

func copyMaterial(material Material) (Material, error) {
	if material.Reference.KeyID.Value == "" || material.Reference.Root.Value == "" || len(material.RootKey) != RootKeyBytes {
		return Material{}, ErrInvalidMaterial
	}
	material.RootKey = append([]byte(nil), material.RootKey...)
	return material, nil
}

func validateTenant(tenant contracts.Identifier) error {
	if tenant.Namespace != "tenant" || tenant.Value == "" {
		return ErrInvalidMaterial
	}
	return nil
}
