// Package changeset validates the frozen Stage 05 atomic candidate edit set.
// It mirrors the ChangeSetPreview and PreviewEdit invariants of
// packages/contracts/proto/ouroboros/contracts/v1/factory.proto: normalized
// repository-relative paths, unique post-image and pre-image paths, exact
// per-operation before/after digest shapes, and rename-only old paths.
// The package is pure: it performs no filesystem or Git access.
package changeset

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// ErrInvalid is the single non-disclosing rejection for a malformed edit set.
var ErrInvalid = errors.New("changeset: invalid edit set")

const (
	// MaxEdits is the frozen per-candidate edit bound.
	MaxEdits = 64
	// MaxPathBytes is the frozen per-path byte bound.
	MaxPathBytes = 4096
)

// Operation is the bounded per-file candidate edit vocabulary.
type Operation string

const (
	// OpAdd creates one new repository-relative file.
	OpAdd Operation = "add"
	// OpModify edits one existing file in place.
	OpModify Operation = "modify"
	// OpDelete removes one existing file.
	OpDelete Operation = "delete"
	// OpRename moves one existing file.
	OpRename Operation = "rename"
)

// Language is one bounded P5 language lane.
type Language string

const (
	LanguageGo         Language = "go"
	LanguageTypeScript Language = "typescript"
	LanguagePython     Language = "python"
	LanguageRust       Language = "rust"
	LanguageJava       Language = "java"
)

// Edit is one normalized per-file candidate edit. NewContent carries the exact
// post-image bytes for add, modify, and rename edits and is empty for delete.
type Edit struct {
	Path         string
	OldPath      string
	Op           Operation
	Lang         Language
	BeforeDigest contracts.Digest
	AfterDigest  contracts.Digest
	NewContent   []byte
}

// DigestBytes returns the canonical sha256 digest binding immutable content.
func DigestBytes(content []byte) contracts.Digest {
	sum := sha256.Sum256(content)
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

// Validate enforces every frozen ChangeSetPreview edit invariant over one
// candidate edit set. Any violation returns ErrInvalid; the reason detail is
// available through Reason for trace only and never crosses the public edge.
func Validate(edits []Edit) error {
	if len(edits) == 0 || len(edits) > MaxEdits {
		return fmt.Errorf("edit count %d: %w", len(edits), ErrInvalid)
	}
	post := make(map[string]struct{}, len(edits))
	pre := make(map[string]struct{}, len(edits))
	for _, edit := range edits {
		if err := ValidateEdit(edit); err != nil {
			return err
		}
		if _, duplicate := post[edit.Path]; duplicate {
			return fmt.Errorf("duplicate post-image path %q: %w", edit.Path, ErrInvalid)
		}
		post[edit.Path] = struct{}{}
		if edit.Op == OpAdd {
			continue
		}
		preimage := edit.Path
		if edit.Op == OpRename {
			preimage = edit.OldPath
		}
		if _, duplicate := pre[preimage]; duplicate {
			return fmt.Errorf("duplicate pre-image path %q: %w", preimage, ErrInvalid)
		}
		pre[preimage] = struct{}{}
	}
	return nil
}

// ValidateEdit enforces the frozen per-edit invariants: normalized paths,
// rename-only old path, per-operation digest shape, a bounded language lane,
// and after-digest equality with the carried post-image bytes.
func ValidateEdit(edit Edit) error {
	if err := ValidatePath(edit.Path); err != nil {
		return err
	}
	switch edit.Op {
	case OpAdd, OpModify, OpDelete:
		if edit.OldPath != "" {
			return fmt.Errorf("operation %s carries an old path: %w", edit.Op, ErrInvalid)
		}
	case OpRename:
		if edit.OldPath == "" || edit.OldPath == edit.Path {
			return fmt.Errorf("rename without a distinct old path: %w", ErrInvalid)
		}
		if err := ValidatePath(edit.OldPath); err != nil {
			return err
		}
	default:
		return fmt.Errorf("operation %q: %w", edit.Op, ErrInvalid)
	}
	if !validLanguage(edit.Lang) {
		return fmt.Errorf("language %q: %w", edit.Lang, ErrInvalid)
	}
	hasBefore := edit.BeforeDigest.Hex != ""
	hasAfter := edit.AfterDigest.Hex != ""
	switch edit.Op {
	case OpAdd:
		if hasBefore || !hasAfter {
			return fmt.Errorf("add digest shape: %w", ErrInvalid)
		}
	case OpDelete:
		if !hasBefore || hasAfter {
			return fmt.Errorf("delete digest shape: %w", ErrInvalid)
		}
	case OpModify, OpRename:
		if !hasBefore || !hasAfter {
			return fmt.Errorf("%s digest shape: %w", edit.Op, ErrInvalid)
		}
	}
	if hasBefore && !validDigest(edit.BeforeDigest) {
		return fmt.Errorf("before digest: %w", ErrInvalid)
	}
	if hasAfter && !validDigest(edit.AfterDigest) {
		return fmt.Errorf("after digest: %w", ErrInvalid)
	}
	if edit.Op == OpDelete {
		if len(edit.NewContent) != 0 {
			return fmt.Errorf("delete carries post-image bytes: %w", ErrInvalid)
		}
		return nil
	}
	if DigestBytes(edit.NewContent) != edit.AfterDigest {
		return fmt.Errorf("post-image bytes do not match the after digest: %w", ErrInvalid)
	}
	return nil
}

// ValidatePath enforces the frozen normalized repository-relative path rule:
// no absolute or parent traversal, no empty, dot, or dot-dot segments, no
// backslash or control characters, and a bounded byte length.
func ValidatePath(value string) error {
	if value == "" || len(value) > MaxPathBytes || !utf8.ValidString(value) ||
		path.IsAbs(value) || strings.Contains(value, "\\") || path.Clean(value) != value {
		return fmt.Errorf("path %q: %w", value, ErrInvalid)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("path %q segment: %w", value, ErrInvalid)
		}
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("path %q control character: %w", value, ErrInvalid)
		}
	}
	return nil
}

func validLanguage(language Language) bool {
	switch language {
	case LanguageGo, LanguageTypeScript, LanguagePython, LanguageRust, LanguageJava:
		return true
	}
	return false
}

func validDigest(digest contracts.Digest) bool {
	if digest.Algorithm != "sha256" || len(digest.Hex) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(digest.Hex)
	return err == nil && hex.EncodeToString(decoded) == digest.Hex
}
