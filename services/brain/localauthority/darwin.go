//go:build darwin

package localauthority

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstorage"
)

// OpenDarwin opens a durable local authority whose SQLite key references are
// resolved only through existing macOS Keychain generic-password items. It uses
// the fixed /usr/bin/security runner and never creates or mutates an item.
// Malformed input returns ErrInvalid; missing material and startup failures
// return ErrUnavailable after all partially opened resources are released.
func OpenDarwin(ctx context.Context, config DarwinConfig) (*Runtime, error) {
	return openDarwin(ctx, config, keyring.NewDarwinCommandRunner())
}

func openDarwin(ctx context.Context, config DarwinConfig, runner keyring.CommandRunner) (*Runtime, error) {
	if runner == nil || !validSelector(config.KeychainService, 1024) {
		return nil, ErrInvalid
	}
	keychain, err := keyring.NewDarwinKeychain(config.KeychainService, runner)
	if err != nil {
		return nil, ErrInvalid
	}
	bootstrap, err := keyring.NewDarwinResolver(staticReferenceSource{config: config.Durable}, keychain)
	if err != nil {
		return nil, ErrInvalid
	}
	return openDurable(ctx, config.Durable, bootstrap, func(
		references *localstorage.KeyReferences,
		commitment KeyReference,
	) (KeyResolver, error) {
		return keyring.NewDarwinResolver(committedReferenceSource{
			source: references, config: config.Durable, commitment: commitment,
		}, keychain)
	})
}

type staticReferenceSource struct {
	config DurableConfig
}

func (s staticReferenceSource) CurrentReference(_ context.Context, tenant Identifier) (KeyReference, error) {
	if tenant != s.config.Tenant {
		return KeyReference{}, keyring.ErrNotFound
	}
	return s.config.CurrentKeyReference, nil
}

func (s staticReferenceSource) Reference(
	_ context.Context,
	tenant Identifier,
	epoch uint64,
) (KeyReference, error) {
	if tenant != s.config.Tenant || epoch != s.config.CurrentKeyReference.Epoch {
		return KeyReference{}, keyring.ErrNotFound
	}
	return s.config.CurrentKeyReference, nil
}

type committedReferenceSource struct {
	source     *localstorage.KeyReferences
	config     DurableConfig
	commitment KeyReference
}

func (s committedReferenceSource) CurrentReference(ctx context.Context, tenant Identifier) (KeyReference, error) {
	reference, err := s.source.CurrentReference(ctx, tenant)
	if err != nil {
		return KeyReference{}, err
	}
	return s.restore(tenant, reference)
}

func (s committedReferenceSource) Reference(
	ctx context.Context,
	tenant Identifier,
	epoch uint64,
) (KeyReference, error) {
	reference, err := s.source.Reference(ctx, tenant, epoch)
	if err != nil {
		return KeyReference{}, err
	}
	if tenant == s.config.Tenant && epoch == s.config.CurrentKeyReference.Epoch {
		return s.restore(tenant, reference)
	}
	return reference, nil
}

func (s committedReferenceSource) restore(tenant Identifier, stored KeyReference) (KeyReference, error) {
	if tenant != s.config.Tenant || stored != s.commitment {
		return KeyReference{}, keyring.ErrInvalidMaterial
	}
	return s.config.CurrentKeyReference, nil
}
