package localbootstrap

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func normalize(manifest BootstrapV1, now time.Time) (*Config, error) {
	if manifest.Version != 1 || !validStatePaths(manifest) ||
		!validOpaque(manifest.Principal, maxIdentifierSize) ||
		!validOpaque(manifest.Tenant, maxIdentifierSize) ||
		!validOpaque(manifest.Session, maxIdentifierSize) ||
		!validOpaque(manifest.Brain, maxIdentifierSize) ||
		!validOpaque(manifest.KeychainService, maxIdentifierSize) ||
		!validOpaque(manifest.KeyReference, maxIdentifierSize) ||
		manifest.KeyEpoch == 0 || manifest.MaxConnections == 0 ||
		manifest.MaxConnections > maxConnections || manifest.MaxRequests == 0 ||
		manifest.MaxRequests > maxRequests || manifest.FrameBytes == 0 ||
		manifest.FrameBytes > maxFrameBytes || manifest.MaxReadBytes == 0 ||
		manifest.MaxReadBytes > maxReadBytes || len(manifest.Relationships) == 0 ||
		len(manifest.Relationships) > maxCollectionSize || len(manifest.IssuedGrants) == 0 ||
		len(manifest.IssuedGrants) > maxCollectionSize {
		return nil, ErrInvalidManifest
	}
	relationships, err := normalizeRelationships(manifest)
	if err != nil {
		return nil, err
	}
	grants, err := normalizeGrants(manifest, now)
	if err != nil {
		return nil, err
	}
	manifest.Relationships = nil
	manifest.IssuedGrants = nil
	return &Config{manifest: manifest, relationships: relationships, issuedGrants: grants}, nil
}

func validStatePaths(manifest BootstrapV1) bool {
	paths := []string{
		manifest.StateRoot,
		manifest.SocketPath,
		manifest.DatabasePath,
		manifest.ObjectRoot,
		manifest.ApprovedSourceRoot,
	}
	for _, path := range paths {
		if !validManifestPath(path) || !validOpaque(path, maxPathSize) {
			return false
		}
	}
	return manifest.DatabasePath == filepath.Join(manifest.StateRoot, databaseLeaf) &&
		manifest.SocketPath == filepath.Join(manifest.StateRoot, socketLeaf) &&
		manifest.ObjectRoot == filepath.Join(manifest.StateRoot, objectLeaf) &&
		!pathsOverlap(manifest.ApprovedSourceRoot, manifest.StateRoot)
}

func normalizeRelationships(manifest BootstrapV1) ([]Relationship, error) {
	relationships := make([]Relationship, 0, len(manifest.Relationships))
	seen := make(map[Relationship]struct{}, len(manifest.Relationships))
	for _, spec := range manifest.Relationships {
		relationship := Relationship(spec)
		if !validRelationship(relationship, manifest) || hasForbiddenAuthorityToken(relationship.Object) ||
			hasForbiddenAuthorityToken(relationship.Relation) || hasForbiddenAuthorityToken(relationship.User) {
			return nil, ErrInvalidManifest
		}
		if _, exists := seen[relationship]; exists {
			return nil, ErrInvalidManifest
		}
		seen[relationship] = struct{}{}
		relationships = append(relationships, relationship)
	}
	sort.Slice(relationships, func(left, right int) bool {
		if relationships[left].Object != relationships[right].Object {
			return relationships[left].Object < relationships[right].Object
		}
		if relationships[left].Relation != relationships[right].Relation {
			return relationships[left].Relation < relationships[right].Relation
		}
		return relationships[left].User < relationships[right].User
	})
	return relationships, nil
}

func validRelationship(relationship Relationship, manifest BootstrapV1) bool {
	if !validOpaque(relationship.Object, maxIdentifierSize) ||
		!validOpaque(relationship.Relation, 64) || !validOpaque(relationship.User, maxIdentifierSize) {
		return false
	}
	objectNamespace, objectValue, objectOK := splitQualified(relationship.Object)
	userNamespace, userValue, userOK := splitQualified(relationship.User)
	if !objectOK || !userOK {
		return false
	}
	switch relationship.Relation {
	case "brain":
		return objectNamespace == "evidence" && userNamespace == "brain" && userValue == manifest.Brain
	case "tenant":
		return objectNamespace == "brain" && objectValue == manifest.Brain &&
			userNamespace == "tenant" && userValue == manifest.Tenant
	case "owner", "viewer":
		return objectNamespace == "brain" && objectValue == manifest.Brain &&
			userNamespace == "user" && userValue == manifest.Principal
	default:
		return false
	}
}

func normalizeGrants(manifest BootstrapV1, now time.Time) ([]IssuedGrant, error) {
	grants := make([]IssuedGrant, 0, len(manifest.IssuedGrants))
	seenIDs := make(map[string]struct{}, len(manifest.IssuedGrants))
	for _, spec := range manifest.IssuedGrants {
		if _, exists := seenIDs[spec.ID]; exists {
			return nil, ErrInvalidManifest
		}
		grant, err := normalizeGrant(spec, manifest, now)
		if err != nil {
			return nil, err
		}
		seenIDs[spec.ID] = struct{}{}
		grants = append(grants, grant)
	}
	sort.Slice(grants, func(left, right int) bool { return grants[left].ID < grants[right].ID })
	return grants, nil
}

func normalizeGrant(spec GrantSpec, manifest BootstrapV1, now time.Time) (IssuedGrant, error) {
	// Preserve the nil check as a local fail-closed invariant even though the
	// current JSON schema rejects absent and null limits before normalization.
	if !validOpaque(spec.ID, maxIdentifierSize) || hasForbiddenAuthorityToken(spec.ID) ||
		!validOpaque(spec.Nonce, 128) || hasForbiddenAuthorityToken(spec.Nonce) ||
		spec.Evidence.Namespace != "evidence" || !validOpaque(spec.Evidence.Value, maxIdentifierSize) ||
		hasForbiddenAuthorityToken(spec.Evidence.Value) || spec.Fence == 0 ||
		spec.RevocationEpoch != manifest.RevocationEpoch || spec.Limits == nil ||
		!validGrantLimits(spec.Action, spec.Limits, manifest) {
		return IssuedGrant{}, ErrInvalidManifest
	}
	if spec.Action != "artifact.admit" && spec.Action != "artifact.read" && spec.Action != "artifact.delete" {
		return IssuedGrant{}, ErrInvalidManifest
	}
	if !strings.HasSuffix(spec.ExpiresAt, "Z") {
		return IssuedGrant{}, ErrInvalidManifest
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, spec.ExpiresAt)
	if err != nil || expiresAt.IsZero() || !now.Before(expiresAt) {
		return IssuedGrant{}, ErrInvalidManifest
	}
	limits := make(map[string]uint64, len(spec.Limits))
	for name, maximum := range spec.Limits {
		limits[name] = maximum
	}
	return IssuedGrant{
		ID: spec.ID, Action: spec.Action, Evidence: spec.Evidence, Fence: spec.Fence,
		Nonce: spec.Nonce, ExpiresAt: expiresAt.UTC(), RevocationEpoch: spec.RevocationEpoch,
		Limits: limits,
	}, nil
}

func validGrantLimits(action string, limits map[string]uint64, manifest BootstrapV1) bool {
	switch action {
	case "artifact.admit":
		return len(limits) == 2 && limits["bytes"] > 0 && limits["bytes"] <= maxAdmitBytes &&
			limits["frames"] > 0 && limits["frames"] <= maxAdmitFrames
	case "artifact.read":
		return len(limits) == 1 && limits["bytes"] > 0 && limits["bytes"] <= manifest.MaxReadBytes
	case "artifact.delete":
		return len(limits) == 0
	default:
		return false
	}
}

func splitQualified(value string) (string, string, bool) {
	namespace, identifier, ok := strings.Cut(value, ":")
	return namespace, identifier, ok && namespace != "" && identifier != "" && !strings.Contains(identifier, ":")
}

func validOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func hasForbiddenAuthorityToken(value string) bool {
	return strings.Contains(value, "*") || strings.Contains(strings.ToLower(value), "delegate")
}
