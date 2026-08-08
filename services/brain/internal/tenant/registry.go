package tenant

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Record is one tenant.
type Record struct {
	ID        string    `json:"id"`
	Region    string    `json:"region,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// CryptoRoot is a non-secret handle (not the key material).
	CryptoRoot string `json:"crypto_root"`
}

// Registry is a file-backed tenant registry under a data root.
type Registry struct {
	mu   sync.Mutex
	Root string // e.g. ~/.ouroboros/tenants or test temp
}

type fileShape struct {
	Tenants map[string]Record `json:"tenants"`
}

func (r *Registry) path() string {
	return filepath.Join(r.Root, "tenants.json")
}

func (r *Registry) load() (fileShape, error) {
	var f fileShape
	raw, err := os.ReadFile(r.path())
	if err != nil {
		if os.IsNotExist(err) {
			return fileShape{Tenants: map[string]Record{}}, nil
		}
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	if f.Tenants == nil {
		f.Tenants = map[string]Record{}
	}
	return f, nil
}

func (r *Registry) save(f fileShape) error {
	if err := os.MkdirAll(r.Root, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path(), raw, 0o600)
}

// validTenantID rejects empty, path separators, and ".." traversal segments.
func validTenantID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.Contains(id, "..") {
		return false
	}
	if strings.Contains(id, "/") || strings.Contains(id, `\`) {
		return false
	}
	if strings.ContainsRune(id, os.PathSeparator) {
		return false
	}
	if filepath.Base(id) != id {
		return false
	}
	return true
}

// Create registers a tenant (idempotent if same id exists and not disabled).
func (r *Registry) Create(id, region string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	if !validTenantID(id) {
		return Record{}, fmt.Errorf("tenant: invalid id")
	}
	f, err := r.load()
	if err != nil {
		return Record{}, err
	}
	if existing, ok := f.Tenants[id]; ok {
		if existing.Disabled {
			return Record{}, fmt.Errorf("tenant: disabled")
		}
		return existing, nil
	}
	rec := Record{
		ID: id, Region: region, CreatedAt: time.Now().UTC(),
		CryptoRoot: "kr-" + id,
	}
	f.Tenants[id] = rec
	if err := r.save(f); err != nil {
		return Record{}, err
	}
	// Tenant home for brains.
	if err := os.MkdirAll(r.BrainRoot(id), 0o700); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// Status returns tenant or error if missing/disabled.
func (r *Registry) Status(id string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return Record{}, err
	}
	rec, ok := f.Tenants[strings.TrimSpace(id)]
	if !ok {
		return Record{}, fmt.Errorf("tenant: not found")
	}
	if rec.Disabled {
		return Record{}, fmt.Errorf("tenant: disabled")
	}
	return rec, nil
}

// Disable marks tenant disabled (fail-closed).
func (r *Registry) Disable(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	rec, ok := f.Tenants[id]
	if !ok {
		return fmt.Errorf("tenant: not found")
	}
	rec.Disabled = true
	f.Tenants[id] = rec
	return r.save(f)
}

// List returns all non-disabled tenants.
func (r *Registry) List() ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, rec := range f.Tenants {
		if !rec.Disabled {
			out = append(out, rec)
		}
	}
	return out, nil
}

// BrainRoot is the directory holding brains for a tenant.
func (r *Registry) BrainRoot(tenantID string) string {
	return filepath.Join(r.Root, "tenants", tenantID, "brains")
}

// BrainDir returns absolute brain path under tenant.
func (r *Registry) BrainDir(tenantID, brainID string) string {
	return filepath.Join(r.BrainRoot(tenantID), brainID)
}

// resolvePathAbs returns an absolute path, preferring symlink resolution when the
// path (or an existing ancestor) can be evaluated.
func resolvePathAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// Path may not exist yet: resolve the longest existing prefix, then rejoin.
	cur := abs
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, suffix), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		base := filepath.Base(cur)
		if suffix == "" {
			suffix = base
		} else {
			suffix = filepath.Join(base, suffix)
		}
		cur = parent
	}
}

// AuthorizeBrainPath ensures path is under tenant brain root (isolation TEN-005).
// Uses EvalSymlinks when possible and filepath.Rel (not string HasPrefix) so
// symlink escapes and sibling-prefix tricks are denied.
func (r *Registry) AuthorizeBrainPath(tenantID, path string) error {
	if _, err := r.Status(tenantID); err != nil {
		return err
	}
	want, err := resolvePathAbs(r.BrainRoot(tenantID))
	if err != nil {
		return err
	}
	got, err := resolvePathAbs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(want, got)
	if err != nil {
		return ErrCrossTenant
	}
	// rel == "." is the brain root itself (allowed); ".." / "../..." escape.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrCrossTenant
	}
	if filepath.IsAbs(rel) {
		return ErrCrossTenant
	}
	return nil
}

// ErrCrossTenant is fail-closed isolation error.
var ErrCrossTenant = fmt.Errorf("tenant: cross-tenant access denied")
