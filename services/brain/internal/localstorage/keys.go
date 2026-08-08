package localstorage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// KeyReferences reads tenant-scoped key metadata for composition with a secret
// material provider such as Darwin Keychain. It has no key-material write or
// fallback surface and returns unreadable epochs as a fail-closed typed error.
type KeyReferences struct {
	authority *localstate.Store
}

type keyRow struct {
	epoch uint64
	keyID string
	state string
}

// InstallCurrentReference creates the tenant's sole current key-reference row.
// Exact retries are no-ops. It returns keyring.ErrInvalidMaterial for malformed
// namespaces or selectors, keyring.ErrKeyConflict for any different existing
// epoch/reference/state, context cancellation unchanged, and ErrUnavailable
// for a durable authority failure. It persists metadata only, never key bytes.
func (r *KeyReferences) InstallCurrentReference(
	ctx context.Context,
	tenant contracts.Identifier,
	reference contracts.KeyReference,
) error {
	if ctx == nil || r == nil || r.authority == nil || !validInstallReference(tenant, reference) {
		return keyring.ErrInvalidMaterial
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeOnly(ctx, r.authority, func(writer localstate.MetadataWriter) error {
		rows, err := writer.QueryContext(ctx, `SELECT key_epoch,key_reference,state FROM key_epochs
			WHERE tenant_id=? AND state='current' ORDER BY key_epoch LIMIT 2`, tenant.Value)
		if err != nil {
			return operationError(ctx, "inspect current key reference")
		}
		defer rows.Close()
		current := make([]keyRow, 0, 2)
		for rows.Next() {
			var row keyRow
			if err := rows.Scan(&row.epoch, &row.keyID, &row.state); err != nil {
				return operationError(ctx, "scan current key reference")
			}
			current = append(current, row)
		}
		if err := rows.Err(); err != nil {
			return operationError(ctx, "iterate current key references")
		}
		if len(current) != 0 {
			if len(current) == 1 && current[0].epoch == reference.Epoch &&
				current[0].keyID == reference.KeyID.Value {
				return nil
			}
			return keyring.ErrKeyConflict
		}

		var existing keyRow
		err = writer.QueryRowContext(ctx, `SELECT key_epoch,key_reference,state FROM key_epochs
			WHERE tenant_id=? AND key_epoch=?`, tenant.Value, reference.Epoch).
			Scan(&existing.epoch, &existing.keyID, &existing.state)
		if err == nil {
			return keyring.ErrKeyConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return operationError(ctx, "inspect key epoch")
		}
		if _, err := writer.ExecContext(ctx, `INSERT INTO key_epochs
			(tenant_id,key_epoch,key_reference,state) VALUES (?,?,?,'current')`,
			tenant.Value, reference.Epoch, reference.KeyID.Value); err != nil {
			return operationError(ctx, "install current key reference")
		}
		return nil
	})
}

// CurrentReference returns the tenant's unique current reference. Missing,
// malformed, and cross-tenant rows fail closed without selecting another epoch.
func (r *KeyReferences) CurrentReference(ctx context.Context, tenant contracts.Identifier) (contracts.KeyReference, error) {
	return r.reference(ctx, tenant, 0, true)
}

// Reference returns one exact current, historical, or legacy key reference.
// Known unreadable epochs return keyring.ErrUnreadable; no material is loaded.
func (r *KeyReferences) Reference(ctx context.Context, tenant contracts.Identifier, epoch uint64) (contracts.KeyReference, error) {
	if epoch == 0 {
		return contracts.KeyReference{}, keyring.ErrInvalidMaterial
	}
	return r.reference(ctx, tenant, epoch, false)
}

func (r *KeyReferences) reference(ctx context.Context, tenant contracts.Identifier, epoch uint64, current bool) (contracts.KeyReference, error) {
	if r == nil || r.authority == nil || !validID(tenant, "tenant") {
		return contracts.KeyReference{}, keyring.ErrInvalidMaterial
	}
	return readResult(ctx, r.authority, func(reader queryer) (contracts.KeyReference, error) {
		if current {
			rows, err := reader.QueryContext(ctx, `SELECT key_epoch,key_reference,state FROM key_epochs
				WHERE tenant_id=? AND state='current' ORDER BY key_epoch LIMIT 2`, tenant.Value)
			if err != nil {
				return contracts.KeyReference{}, operationError(ctx, "read current key references")
			}
			defer rows.Close()
			matches := make([]keyRow, 0, 2)
			for rows.Next() {
				var match keyRow
				if err := rows.Scan(&match.epoch, &match.keyID, &match.state); err != nil {
					return contracts.KeyReference{}, operationError(ctx, "scan current key reference")
				}
				matches = append(matches, match)
			}
			if err := rows.Err(); err != nil {
				return contracts.KeyReference{}, operationError(ctx, "iterate current key references")
			}
			switch len(matches) {
			case 0:
				return contracts.KeyReference{}, keyring.ErrNotFound
			case 1:
				return validateKeyReference(tenant, matches[0])
			default:
				return contracts.KeyReference{}, keyring.ErrInvalidMaterial
			}
		}
		var keyID, state string
		var storedEpoch uint64
		err := reader.QueryRowContext(ctx, `SELECT key_epoch,key_reference,state FROM key_epochs
			WHERE tenant_id=? AND key_epoch=?`, tenant.Value, epoch).Scan(&storedEpoch, &keyID, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.KeyReference{}, keyring.ErrNotFound
		}
		if err != nil {
			return contracts.KeyReference{}, operationError(ctx, "read key reference")
		}
		return validateKeyReference(tenant, keyRow{epoch: storedEpoch, keyID: keyID, state: state})
	})
}

func validateKeyReference(tenant contracts.Identifier, row keyRow) (contracts.KeyReference, error) {
	if row.epoch == 0 || !validKeySelector(row.keyID) {
		return contracts.KeyReference{}, keyring.ErrInvalidMaterial
	}
	if row.state == string(keyring.Unreadable) {
		return contracts.KeyReference{}, keyring.ErrUnreadable
	}
	if row.state != string(keyring.Current) && row.state != string(keyring.Historical) && row.state != string(keyring.Legacy) {
		return contracts.KeyReference{}, keyring.ErrInvalidMaterial
	}
	return contracts.KeyReference{
		Root:  contracts.Identifier{Namespace: "key-root", Value: tenant.Value},
		KeyID: contracts.Identifier{Namespace: "key", Value: row.keyID},
		Epoch: row.epoch, Legacy: row.state == string(keyring.Legacy),
	}, nil
}

func validInstallReference(tenant contracts.Identifier, reference contracts.KeyReference) bool {
	return validID(tenant, "tenant") && !containsControl(tenant.Value) &&
		reference.Root.Namespace == "key-root" && reference.Root.Value == tenant.Value &&
		reference.Epoch > 0 && !reference.Legacy &&
		reference.KeyID.Namespace == "key" && validKeySelector(reference.KeyID.Value)
}

func validKeySelector(value string) bool {
	return value != "" && len(value) <= 1024 && !containsControl(value)
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}
