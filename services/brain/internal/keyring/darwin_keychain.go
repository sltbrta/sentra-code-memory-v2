//go:build darwin

package keyring

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrInvalidReference reports an empty or control-character-bearing Keychain selector.
	ErrInvalidReference = errors.New("keyring: invalid keychain reference")
	// ErrKeychainUnavailable reports a static production Keychain operation failure.
	ErrKeychainUnavailable = errors.New("keyring: keychain unavailable")
	// ErrKeychainItemNotFound distinguishes an absent immutable key from Keychain outage.
	ErrKeychainItemNotFound = errors.New("keyring: keychain item not found")
)

// KeychainReference selects one generic-password item without carrying its value.
type KeychainReference struct {
	Account string
}

// CommandRunner executes the fixed macOS security binary. Tests inject a recorder;
// production callers should use NewDarwinCommandRunner.
type CommandRunner interface {
	Run(context.Context, []string, []byte) ([]byte, error)
}

// DarwinKeychain stores root material in macOS Keychain without a file fallback.
// Store sends base64 material over stdin so secrets never appear in process arguments.
type DarwinKeychain struct {
	service string
	runner  CommandRunner
}

// ReferenceSource resolves persisted key metadata without material bytes.
// The Stage 2 SQLite adapter implements this boundary during composition.
type ReferenceSource interface {
	CurrentReference(context.Context, contracts.Identifier) (contracts.KeyReference, error)
	Reference(context.Context, contracts.Identifier, uint64) (contracts.KeyReference, error)
}

// DarwinResolver combines SQLite key metadata with macOS Keychain material.
type DarwinResolver struct {
	references ReferenceSource
	keychain   *DarwinKeychain
}

// NewDarwinResolver constructs the production Resolver without a file fallback.
func NewDarwinResolver(references ReferenceSource, keychain *DarwinKeychain) (*DarwinResolver, error) {
	if references == nil || keychain == nil {
		return nil, ErrInvalidReference
	}
	return &DarwinResolver{references: references, keychain: keychain}, nil
}

// Current resolves the current reference and loads its exact Keychain item.
func (r *DarwinResolver) Current(ctx context.Context, tenant contracts.Identifier) (Material, error) {
	if err := validateTenant(tenant); err != nil {
		return Material{}, err
	}
	reference, err := r.references.CurrentReference(ctx, tenant)
	if err != nil {
		return Material{}, ErrNotFound
	}
	return r.load(ctx, tenant, reference)
}

// Resolve loads current, historical, or legacy material for an exact metadata epoch.
func (r *DarwinResolver) Resolve(ctx context.Context, tenant contracts.Identifier, epoch uint64) (Material, error) {
	if err := validateTenant(tenant); err != nil {
		return Material{}, err
	}
	reference, err := r.references.Reference(ctx, tenant, epoch)
	if err != nil {
		return Material{}, ErrNotFound
	}
	return r.load(ctx, tenant, reference)
}

func (r *DarwinResolver) load(ctx context.Context, tenant contracts.Identifier, reference contracts.KeyReference) (Material, error) {
	if reference.Root.Namespace != "key-root" || reference.Root.Value != tenant.Value || reference.KeyID.Namespace != "key" || !validSelector(reference.KeyID.Value) {
		return Material{}, ErrInvalidMaterial
	}
	root, err := r.keychain.Load(ctx, KeychainReference{Account: encodeAccount(tenant.Value, reference.KeyID.Value)})
	if err != nil {
		return Material{}, ErrUnreadable
	}
	return Material{Reference: reference, RootKey: root}, nil
}

// NewDarwinKeychain validates the fixed service namespace and runner.
func NewDarwinKeychain(service string, runner CommandRunner) (*DarwinKeychain, error) {
	if !validSelector(service) || runner == nil {
		return nil, ErrInvalidReference
	}
	return &DarwinKeychain{service: service, runner: runner}, nil
}

// Store creates one immutable root item. Exact retries succeed; conflicting retries fail.
func (k *DarwinKeychain) Store(ctx context.Context, ref KeychainReference, root []byte) error {
	if !validSelector(ref.Account) || len(root) != RootKeyBytes {
		return ErrInvalidReference
	}
	existing, err := k.Load(ctx, ref)
	if err == nil {
		defer clear(existing)
		if subtle.ConstantTimeCompare(existing, root) == 1 {
			return nil
		}
		return ErrKeyConflict
	}
	if !errors.Is(err, ErrKeychainItemNotFound) {
		return ErrKeychainUnavailable
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(root))+1)
	base64.StdEncoding.Encode(encoded, root)
	encoded[len(encoded)-1] = '\n'
	_, err = k.runner.Run(ctx, []string{
		"add-generic-password", "-a", ref.Account, "-s", k.service, "-w",
	}, encoded)
	clear(encoded)
	if err == nil {
		return nil
	}
	// Another creator may have won after the absence check. Accept only its exact key.
	existing, loadErr := k.Load(ctx, ref)
	if loadErr != nil {
		return ErrKeychainUnavailable
	}
	defer clear(existing)
	if subtle.ConstantTimeCompare(existing, root) != 1 {
		return ErrKeyConflict
	}
	return nil
}

// Load retrieves and decodes one root item. It never includes command output in errors.
func (k *DarwinKeychain) Load(ctx context.Context, ref KeychainReference) ([]byte, error) {
	if !validSelector(ref.Account) {
		return nil, ErrInvalidReference
	}
	output, err := k.runner.Run(ctx, []string{
		"find-generic-password", "-a", ref.Account, "-s", k.service, "-w",
	}, nil)
	if errors.Is(err, ErrKeychainItemNotFound) {
		clear(output)
		return nil, ErrKeychainItemNotFound
	}
	if err != nil {
		clear(output)
		return nil, ErrKeychainUnavailable
	}
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) != base64.StdEncoding.EncodedLen(RootKeyBytes) {
		clear(output)
		return nil, ErrKeychainUnavailable
	}
	decoded := make([]byte, RootKeyBytes)
	decodedBytes, err := base64.StdEncoding.Decode(decoded, trimmed)
	clear(output)
	if err != nil || decodedBytes != RootKeyBytes {
		clear(decoded)
		return nil, ErrKeychainUnavailable
	}
	return decoded, nil
}

// Delete removes one exact item. Missing and denied items return the same static error.
func (k *DarwinKeychain) Delete(ctx context.Context, ref KeychainReference) error {
	if !validSelector(ref.Account) {
		return ErrInvalidReference
	}
	_, err := k.runner.Run(ctx, []string{
		"delete-generic-password", "-a", ref.Account, "-s", k.service,
	}, nil)
	if err != nil {
		return ErrKeychainUnavailable
	}
	return nil
}

func validSelector(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}

func encodeAccount(parts ...string) string {
	var account strings.Builder
	for _, part := range parts {
		account.WriteString(strconv.Itoa(len(part)))
		account.WriteByte(':')
		account.WriteString(part)
	}
	return account.String()
}

type commandRunner struct{}

// NewDarwinCommandRunner returns the production explicit-argument security runner.
func NewDarwinCommandRunner() CommandRunner {
	return commandRunner{}
}

func (commandRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, "/usr/bin/security", args...)
	command.Stdin = bytes.NewReader(stdin)
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 44 {
			return nil, ErrKeychainItemNotFound
		}
		return nil, ErrKeychainUnavailable
	}
	return output, nil
}
