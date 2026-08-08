package factory

import (
	"encoding/json"
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// validatePreview enforces the frozen ChangeSetPreview shape before the
// candidate becomes canonical: normalized safe paths, bounded edit shapes,
// unique post-image and pre-image paths, one obligation per touched language,
// unique gate identifiers, the exact base binding, and leaf-scope attenuation
// of every edit. The marshaled proto is re-validated through protovalidate so
// any divergence from the frozen CEL rules fails closed.
func (k *Kernel) validatePreview(preview *contractsv1.ChangeSetPreview, run runRow, leafScopes []string) error {
	if preview.GetChangeSet() == nil || preview.GetChangeSet().GetChangeSetId().GetValue() == "" {
		return fmt.Errorf("%w: candidate identity missing", ErrPlanInvalid)
	}
	if preview.GetCandidateState() != contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED {
		return fmt.Errorf("%w: candidates are proposed only in PROPOSED state", ErrPlanInvalid)
	}
	if preview.GetExpectedBaseGitOid() != run.repositoryGitOID ||
		preview.GetChangeSet().GetBaseGitOid() != run.repositoryGitOID {
		return fmt.Errorf("%w: candidate base does not equal the admitted intent base", ErrPlanInvalid)
	}
	edits := preview.GetEdits()
	if len(edits) == 0 || len(edits) > 64 {
		return fmt.Errorf("%w: edit count %d outside 1..64", ErrPlanInvalid, len(edits))
	}
	postImages := make(map[string]struct{}, len(edits))
	preImages := make(map[string]struct{}, len(edits))
	languages := make(map[contractsv1.CodeLanguage]struct{}, 5)
	for _, edit := range edits {
		if err := validatePreviewEdit(edit); err != nil {
			return err
		}
		if _, duplicate := postImages[edit.GetPath()]; duplicate {
			return fmt.Errorf("%w: post-image path %q is not unique", ErrPlanInvalid, edit.GetPath())
		}
		postImages[edit.GetPath()] = struct{}{}
		preImage := edit.GetPath()
		if edit.GetOperation() == contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME {
			preImage = edit.GetOldPath()
		}
		if edit.GetOperation() != contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_ADD {
			if _, duplicate := preImages[preImage]; duplicate {
				return fmt.Errorf("%w: pre-image path %q is not unique", ErrPlanInvalid, preImage)
			}
			preImages[preImage] = struct{}{}
		}
		if !pathWithinScope(edit.GetPath(), leafScopes) {
			return fmt.Errorf("%w: edit path %q escapes every leaf scope", ErrScopeEscape, edit.GetPath())
		}
		if edit.GetOperation() == contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME &&
			!pathWithinScope(edit.GetOldPath(), leafScopes) {
			return fmt.Errorf("%w: rename pre-image %q escapes every leaf scope", ErrScopeEscape, edit.GetOldPath())
		}
		languages[edit.GetLanguage()] = struct{}{}
	}
	obligations := preview.GetObligations()
	if len(obligations) != len(languages) {
		return fmt.Errorf("%w: obligations must cover exactly the touched languages", ErrPlanInvalid)
	}
	seenObligations := make(map[contractsv1.CodeLanguage]struct{}, len(obligations))
	for _, obligation := range obligations {
		if _, touched := languages[obligation.GetLanguage()]; !touched {
			return fmt.Errorf("%w: obligation covers an untouched language", ErrPlanInvalid)
		}
		if _, duplicate := seenObligations[obligation.GetLanguage()]; duplicate {
			return fmt.Errorf("%w: duplicate obligation for one language", ErrPlanInvalid)
		}
		seenObligations[obligation.GetLanguage()] = struct{}{}
	}
	if err := k.validatePreviewGates(preview); err != nil {
		return err
	}
	validator, err := protovalidate.New()
	if err != nil {
		return fmt.Errorf("factory: build validator: %w", err)
	}
	if err := validator.Validate(preview); err != nil {
		return fmt.Errorf("%w: %v", ErrPlanInvalid, err)
	}
	return nil
}

// validatePreviewEdit mirrors the frozen preview_edit CEL rules: normalized
// paths, the bounded operation vocabulary, rename shape, and exact
// before/after digests per operation.
func validatePreviewEdit(edit *contractsv1.PreviewEdit) error {
	if edit == nil || !validRepositoryPath(edit.GetPath()) {
		return fmt.Errorf("%w: edit path is not normalized", ErrPlanInvalid)
	}
	if edit.GetLanguage() == contractsv1.CodeLanguage_CODE_LANGUAGE_UNSPECIFIED {
		return fmt.Errorf("%w: edit language is unspecified", ErrPlanInvalid)
	}
	operation := edit.GetOperation()
	switch operation {
	case contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_ADD:
		if edit.GetBeforeDigest() != nil || edit.GetAfterDigest() == nil {
			return fmt.Errorf("%w: add edits carry only an after digest", ErrPlanInvalid)
		}
	case contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_DELETE:
		if edit.GetBeforeDigest() == nil || edit.GetAfterDigest() != nil {
			return fmt.Errorf("%w: delete edits carry only a before digest", ErrPlanInvalid)
		}
	case contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_MODIFY,
		contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME:
		if edit.GetBeforeDigest() == nil || edit.GetAfterDigest() == nil {
			return fmt.Errorf("%w: modify and rename edits carry both digests", ErrPlanInvalid)
		}
	default:
		return fmt.Errorf("%w: edit operation is unspecified", ErrPlanInvalid)
	}
	if operation == contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME {
		if edit.GetOldPath() == "" || edit.GetOldPath() == edit.GetPath() || !validRepositoryPath(edit.GetOldPath()) {
			return fmt.Errorf("%w: rename edits carry a distinct normalized old path", ErrPlanInvalid)
		}
	} else if edit.GetOldPath() != "" {
		return fmt.Errorf("%w: only rename edits carry an old path", ErrPlanInvalid)
	}
	for _, digest := range []*contractsv1.Digest{edit.GetBeforeDigest(), edit.GetAfterDigest()} {
		if digest != nil && (digest.GetAlgorithm() != "sha256" || !isHexDigest(digest.GetHex())) {
			return fmt.Errorf("%w: edit digest is not a canonical sha256", ErrPlanInvalid)
		}
	}
	return nil
}

// validatePreviewGates enforces the bounded preview gate roster: unique gate
// identifiers and the four non-removable required gates, matching the plan
// roster the kernel authored at admission.
func (k *Kernel) validatePreviewGates(preview *contractsv1.ChangeSetPreview) error {
	gates := preview.GetGates()
	if len(gates) < 4 || len(gates) > 8 {
		return fmt.Errorf("%w: gate roster cardinality %d outside 4..8", ErrPlanInvalid, len(gates))
	}
	seen := make(map[string]struct{}, len(gates))
	required := make(map[contractsv1.FactoryGateKind]bool, 4)
	for _, gate := range gates {
		key := gate.GetGateId().GetNamespace() + "\x1f" + gate.GetGateId().GetValue()
		if gate.GetGateId().GetValue() == "" {
			return fmt.Errorf("%w: gate identity missing", ErrPlanInvalid)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: gate identity %q is not unique", ErrPlanInvalid, key)
		}
		seen[key] = struct{}{}
		if _, err := gateKindText(gate.GetKind()); err != nil {
			return fmt.Errorf("%w: gate kind invalid", ErrPlanInvalid)
		}
		if _, err := gateStatusText(gate.GetStatus()); err != nil {
			return fmt.Errorf("%w: gate status invalid", ErrPlanInvalid)
		}
		if gate.GetRequired() {
			required[gate.GetKind()] = true
		}
	}
	for _, kind := range []contractsv1.FactoryGateKind{
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
	} {
		if !required[kind] {
			return fmt.Errorf("%w: a non-removable required gate is missing", ErrPlanInvalid)
		}
	}
	return nil
}

// decodePaths parses one schema JSON path column.
func decodePaths(encoded string) ([]string, error) {
	paths := make([]string, 0)
	if err := json.Unmarshal([]byte(encoded), &paths); err != nil {
		return nil, fmt.Errorf("factory: decode path column: %w", err)
	}
	return paths, nil
}

// reviewerDisjoint proves the reviewer identity is disjoint from the
// admitting principal, which initiates every leaf grant.
func reviewerDisjoint(reviewerPrincipal, admittingPrincipal string) bool {
	return reviewerPrincipal != "" && !strings.EqualFold(reviewerPrincipal, admittingPrincipal)
}
