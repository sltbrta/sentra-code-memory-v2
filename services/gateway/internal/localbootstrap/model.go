package localbootstrap

import "time"

const (
	// MaxManifestBytes is the largest accepted bootstrap document.
	MaxManifestBytes  = 256 * 1024
	maxCollectionSize = 4096
	maxIdentifierSize = 512
	maxPathSize       = 4096
	maxConnections    = 4096
	maxRequests       = 1_000_000
	maxFrameBytes     = 16 * 1024 * 1024
	maxReadBytes      = 16 * 1024 * 1024
	maxAdmitBytes     = 1 << 40
	maxAdmitFrames    = 1 << 20
)

// Options names every ambient fact needed to load a bootstrap manifest.
// ManifestPath and ExpectedSHA256 must be explicit; Now is injected so expiry
// decisions are deterministic. Load returns an error when any field is absent.
type Options struct {
	ManifestPath   string
	ExpectedSHA256 string
	Now            func() time.Time
}

// BootstrapV1 is the strict non-secret JSON representation accepted by Load.
// It contains references and bounded policy facts, never keys or source bytes.
type BootstrapV1 struct {
	Version            int                `json:"version"`
	StateRoot          string             `json:"state_root"`
	SocketPath         string             `json:"socket_path"`
	DatabasePath       string             `json:"database_path"`
	ObjectRoot         string             `json:"object_root"`
	ApprovedSourceRoot string             `json:"approved_source_root"`
	Principal          string             `json:"principal"`
	Tenant             string             `json:"tenant"`
	Session            string             `json:"session"`
	Brain              string             `json:"brain"`
	KeychainService    string             `json:"keychain_service"`
	KeyEpoch           uint64             `json:"key_epoch"`
	KeyReference       string             `json:"key_reference"`
	MaxConnections     uint32             `json:"max_connections"`
	MaxRequests        uint32             `json:"max_requests"`
	FrameBytes         uint32             `json:"frame_bytes"`
	MaxReadBytes       uint64             `json:"max_read_bytes"`
	RevocationEpoch    uint64             `json:"revocation_epoch"`
	Relationships      []RelationshipSpec `json:"relationships"`
	IssuedGrants       []GrantSpec        `json:"issued_grants"`
}

// RelationshipSpec is one local OpenFGA-shaped object/relation/user fact.
type RelationshipSpec struct {
	Object   string `json:"object"`
	Relation string `json:"relation"`
	User     string `json:"user"`
}

// EvidenceSpec identifies the only resource namespace accepted by local grants.
type EvidenceSpec struct {
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
}

// GrantSpec is one non-delegable, single-action bootstrap grant.
type GrantSpec struct {
	ID              string            `json:"id"`
	Action          string            `json:"action"`
	Evidence        EvidenceSpec      `json:"evidence"`
	Fence           uint64            `json:"fence"`
	Nonce           string            `json:"nonce"`
	ExpiresAt       string            `json:"expires_at"`
	RevocationEpoch uint64            `json:"revocation_epoch"`
	Limits          map[string]uint64 `json:"limits"`
}

// Relationship is one validated and normalized relationship fact.
type Relationship struct {
	Object   string
	Relation string
	User     string
}

// IssuedGrant is one validated and normalized executable capability fact.
// Limits belongs to the returned value; Config never exposes its stored map.
type IssuedGrant struct {
	ID              string
	Action          string
	Evidence        EvidenceSpec
	Fence           uint64
	Nonce           string
	ExpiresAt       time.Time
	RevocationEpoch uint64
	Limits          map[string]uint64
}

// Config is an immutable normalized bootstrap configuration. Collection
// accessors return copies, so callers cannot mutate the loader's accepted state.
type Config struct {
	manifest         BootstrapV1
	relationships    []Relationship
	issuedGrants     []IssuedGrant
	configurationSHA string
	policySHA        string
}

// StateRoot returns the canonical owner-only local authority state directory.
// Downstream composition must retain a descriptor for this directory and open
// its fixed children relative to that descriptor with no-follow semantics.
func (c *Config) StateRoot() string { return c.manifest.StateRoot }

// SocketPath returns the absolute owner-only Unix socket path.
func (c *Config) SocketPath() string { return c.manifest.SocketPath }

// DatabasePath returns the absolute local authority SQLite path.
func (c *Config) DatabasePath() string { return c.manifest.DatabasePath }

// ObjectRoot returns the absolute encrypted object directory.
func (c *Config) ObjectRoot() string { return c.manifest.ObjectRoot }

// ApprovedSourceRoot returns the sole preapproved local Git root for bounded v1.
// Callers must retain a directory descriptor and apply no-follow traversal.
func (c *Config) ApprovedSourceRoot() string { return c.manifest.ApprovedSourceRoot }

// Principal returns the configured local principal value.
func (c *Config) Principal() string { return c.manifest.Principal }

// Tenant returns the configured local tenant value.
func (c *Config) Tenant() string { return c.manifest.Tenant }

// Session returns the configured local session value.
func (c *Config) Session() string { return c.manifest.Session }

// Brain returns the configured local brain value.
func (c *Config) Brain() string { return c.manifest.Brain }

// KeychainService returns the non-secret Keychain service name.
func (c *Config) KeychainService() string { return c.manifest.KeychainService }

// KeyEpoch returns the required encryption key epoch.
func (c *Config) KeyEpoch() uint64 { return c.manifest.KeyEpoch }

// KeyReference returns the opaque non-secret Keychain reference.
func (c *Config) KeyReference() string { return c.manifest.KeyReference }

// MaxConnections returns the bounded active connection limit.
func (c *Config) MaxConnections() uint32 { return c.manifest.MaxConnections }

// MaxRequests returns the bounded per-connection request limit.
func (c *Config) MaxRequests() uint32 { return c.manifest.MaxRequests }

// FrameBytes returns the encrypted artifact frame size.
func (c *Config) FrameBytes() uint32 { return c.manifest.FrameBytes }

// MaxReadBytes returns the maximum bytes returned by one artifact read.
func (c *Config) MaxReadBytes() uint64 { return c.manifest.MaxReadBytes }

// RevocationEpoch returns the current local deny-overlay epoch.
func (c *Config) RevocationEpoch() uint64 { return c.manifest.RevocationEpoch }

// ConfigurationDigest returns SHA-256 over the exact accepted manifest bytes.
func (c *Config) ConfigurationDigest() string { return c.configurationSHA }

// PolicyDigest returns the deterministic digest over normalized policy facts.
func (c *Config) PolicyDigest() string { return c.policySHA }

// Relationships returns a sorted copy of the accepted relationship facts.
func (c *Config) Relationships() []Relationship {
	return append([]Relationship(nil), c.relationships...)
}

// IssuedGrants returns sorted, deeply copied grant facts.
func (c *Config) IssuedGrants() []IssuedGrant {
	grants := make([]IssuedGrant, len(c.issuedGrants))
	for index, grant := range c.issuedGrants {
		grants[index] = cloneGrant(grant)
	}
	return grants
}

func cloneGrant(grant IssuedGrant) IssuedGrant {
	limits := make(map[string]uint64, len(grant.Limits))
	for name, maximum := range grant.Limits {
		limits[name] = maximum
	}
	grant.Limits = limits
	return grant
}
