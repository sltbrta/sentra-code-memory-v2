package localbootstrap

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"unicode/utf8"
)

var (
	// ErrInvalidOptions reports absent or malformed explicit loader inputs.
	ErrInvalidOptions = errors.New("localbootstrap: invalid options")
	// ErrUnsafeManifest reports a path, owner, type, or permission violation.
	ErrUnsafeManifest = errors.New("localbootstrap: unsafe manifest")
	// ErrDigestMismatch reports bytes that do not match the pinned SHA-256.
	ErrDigestMismatch = errors.New("localbootstrap: manifest digest mismatch")
	// ErrInvalidManifest reports malformed JSON or an invalid bootstrap contract.
	ErrInvalidManifest = errors.New("localbootstrap: invalid manifest")

	lowerSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Load returns an immutable normalized bootstrap configuration after verifying
// owner-only filesystem custody, the expected SHA-256, strict JSON, expiry, and
// all authority bounds. It returns one of the exported sentinel errors and does
// not use environment variables, the working directory, or default paths.
func Load(options Options) (*Config, error) {
	if options.Now == nil || !validManifestPath(options.ManifestPath) ||
		!lowerSHA256Pattern.MatchString(options.ExpectedSHA256) {
		return nil, ErrInvalidOptions
	}
	now := options.Now()
	if now.IsZero() {
		return nil, ErrInvalidOptions
	}
	expected, err := hex.DecodeString(options.ExpectedSHA256)
	if err != nil {
		return nil, ErrInvalidOptions
	}
	payload, err := readOwnerOnly(options.ManifestPath)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(payload)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return nil, ErrDigestMismatch
	}
	manifest, err := decodeManifest(payload)
	if err != nil {
		return nil, ErrInvalidManifest
	}
	if err := validateStateLayout(manifest, options.ManifestPath); err != nil {
		return nil, err
	}
	config, err := normalize(manifest, now.UTC())
	if err != nil {
		return nil, ErrInvalidManifest
	}
	config.configurationSHA = hex.EncodeToString(actual[:])
	config.policySHA = policyDigest(config)
	return config, nil
}

func validManifestPath(path string) bool {
	return path != "" && len(path) <= maxPathSize && filepath.IsAbs(path) &&
		filepath.Clean(path) == path && path != filepath.VolumeName(path)+string(filepath.Separator)
}

func readOwnerOnly(path string) ([]byte, error) {
	parentPath := filepath.Dir(path)
	parent, err := os.Lstat(parentPath)
	if err != nil || !safeParent(parent) || !resolvesToSelf(parentPath) {
		return nil, ErrUnsafeManifest
	}
	entry, err := os.Lstat(path)
	if err != nil || !safeFile(entry) || !resolvesToSelf(path) {
		return nil, ErrUnsafeManifest
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnsafeManifest
	}
	// Closing a read-only descriptor cannot flush data; the read error remains authoritative.
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !safeFile(opened) || !os.SameFile(entry, opened) {
		return nil, ErrUnsafeManifest
	}
	parentAfter, err := os.Lstat(parentPath)
	if err != nil || !safeParent(parentAfter) || !os.SameFile(parent, parentAfter) ||
		!resolvesToSelf(parentPath) || !resolvesToSelf(path) {
		return nil, ErrUnsafeManifest
	}
	payload, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil || len(payload) == 0 || len(payload) > MaxManifestBytes {
		return nil, ErrInvalidManifest
	}
	return payload, nil
}

func resolvesToSelf(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safeParent(info os.FileInfo) bool {
	return info.IsDir() && exactMode(info.Mode(), 0o700) && ownedByCurrentUser(info)
}

func safeFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && exactMode(info.Mode(), 0o600) && ownedByCurrentUser(info)
}

func exactMode(mode os.FileMode, want os.FileMode) bool {
	const securityBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return mode&securityBits == want
}

func decodeManifest(payload []byte) (BootstrapV1, error) {
	if !utf8.Valid(payload) {
		return BootstrapV1{}, ErrInvalidManifest
	}
	if err := validateManifestJSON(payload); err != nil {
		return BootstrapV1{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest BootstrapV1
	if err := decoder.Decode(&manifest); err != nil {
		return BootstrapV1{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return BootstrapV1{}, ErrInvalidManifest
	}
	return manifest, nil
}
