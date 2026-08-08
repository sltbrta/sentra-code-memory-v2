package outcomes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Admissions is an in-process sanitized outcome admission ledger. Production
// composition will wrap ArtifactVault/SQLite; unit tests use this pure store.
type Admissions struct {
	mu    sync.Mutex
	byKey map[string]AdmittedFact // tenant\x00idempotency → fact
	byID  map[string]AdmittedFact // tenant\x00factID → fact
}

// New returns an empty admissions ledger.
func New() *Admissions {
	return &Admissions{
		byKey: make(map[string]AdmittedFact),
		byID:  make(map[string]AdmittedFact),
	}
}

// Admit validates and records one sanitized outcome fact. Exact retries of the
// same tenant+idempotency key with identical digests replay; mismatched reuse
// conflicts. Elevated model output, missing raw-trace separation, or forbidden
// keys fail closed with OUTCOME_SANITIZATION_FAILED and admit nothing.
func (a *Admissions) Admit(request AdmitRequest) (AdmittedFact, error) {
	if a == nil {
		return AdmittedFact{}, fmt.Errorf("%w: admissions is nil", ErrInvalidInput)
	}
	if err := Validate(request); err != nil {
		return AdmittedFact{}, err
	}
	bundleDigest := DigestBundle(request.OutcomeBundle)
	fact := AdmittedFact{
		FactID:               request.FactID,
		AuthorityClass:       AuthorityMachineObservation,
		OutcomeBundleDigest:  bundleDigest,
		DraftPrReceiptDigest: request.DraftPrReceiptDigest,
		Evidence:             append([]EvidenceRef(nil), request.Evidence...),
		RawTraceSeparated:    true,
		Receipt: contracts.Receipt{
			OperationID: contracts.Identifier{Namespace: "outcome-fact", Value: request.FactID},
			Status:      "completed",
			ReasonCode:  ReasonAllowed,
		},
	}
	key := request.Tenant + "\x00" + request.IdempotencyKey
	idKey := request.Tenant + "\x00" + request.FactID

	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.byKey[key]; ok {
		if existing.OutcomeBundleDigest.Hex != fact.OutcomeBundleDigest.Hex ||
			existing.DraftPrReceiptDigest.Hex != fact.DraftPrReceiptDigest.Hex ||
			existing.FactID != fact.FactID {
			return AdmittedFact{}, fmt.Errorf("%w: %s", ErrConflict, ReasonDuplicateMismatch)
		}
		existing.Replayed = true
		return existing, nil
	}
	if existing, ok := a.byID[idKey]; ok {
		if existing.OutcomeBundleDigest.Hex != fact.OutcomeBundleDigest.Hex {
			return AdmittedFact{}, fmt.Errorf("%w: %s", ErrConflict, ReasonDuplicateMismatch)
		}
		existing.Replayed = true
		return existing, nil
	}
	a.byKey[key] = fact
	a.byID[idKey] = fact
	return fact, nil
}

// Get returns an admitted fact by tenant and fact ID.
func (a *Admissions) Get(tenant, factID string) (AdmittedFact, bool) {
	if a == nil {
		return AdmittedFact{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fact, ok := a.byID[tenant+"\x00"+factID]
	return fact, ok
}

// Validate enforces authority class, raw-trace separation, digest shape, and
// sanitizer policy without mutating state.
func Validate(request AdmitRequest) error {
	if request.Tenant == "" || request.Principal == "" || request.FactID == "" {
		return fmt.Errorf("%w: identity incomplete", ErrInvalidInput)
	}
	if request.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency key missing", ErrInvalidInput)
	}
	if len(request.OutcomeBundle) == 0 {
		return fmt.Errorf("%w: outcome bundle empty", ErrInvalidInput)
	}
	if !validDigest(request.DraftPrReceiptDigest) {
		return fmt.Errorf("%w: draft-pr receipt digest malformed", ErrInvalidInput)
	}
	if request.AuthorityClass != AuthorityMachineObservation {
		return fmt.Errorf("%w: %s", ErrSanitizationFailed, ReasonAuthorityClassInvalid)
	}
	if !request.RawTraceSeparated {
		return fmt.Errorf("%w: %s", ErrSanitizationFailed, ReasonRawTraceNotSeparated)
	}
	for _, ref := range request.Evidence {
		if ref.EvidenceID == "" || !validDigest(ref.Digest) {
			return fmt.Errorf("%w: evidence ref malformed", ErrInvalidInput)
		}
		if ref.AuthorityClass != "" && ref.AuthorityClass != AuthorityMachineObservation {
			return fmt.Errorf("%w: evidence authority class %q elevated", ErrSanitizationFailed, ref.AuthorityClass)
		}
	}
	if err := SanitizeCheck(request.OutcomeBundle); err != nil {
		return err
	}
	return nil
}

// SanitizeCheck scans sanitized bundle bytes for forbidden keys and elevated
// model-proposal authority markers. Bundles may be JSON objects or opaque
// digests-only envelopes; non-JSON is accepted only when it contains none of
// the forbidden substrings (case-insensitive).
func SanitizeCheck(bundle []byte) error {
	lower := strings.ToLower(string(bundle))
	for _, key := range ForbiddenKeys {
		if strings.Contains(lower, strings.ToLower(key)) {
			return fmt.Errorf("%w: forbidden field %q: %s", ErrSanitizationFailed, key, ReasonOutcomeSanitizationFail)
		}
	}
	// JSON structure check when parseable.
	var parsed any
	if err := json.Unmarshal(bundle, &parsed); err != nil {
		return nil // non-JSON opaque bytes already substring-scanned
	}
	if err := walkJSON(parsed, ""); err != nil {
		return err
	}
	return nil
}

func walkJSON(value any, path string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			lower := strings.ToLower(key)
			for _, forbidden := range ForbiddenKeys {
				if lower == strings.ToLower(forbidden) {
					return fmt.Errorf("%w: forbidden key %q: %s", ErrSanitizationFailed, key, ReasonOutcomeSanitizationFail)
				}
			}
			if lower == "authority_class" || lower == "authorityclass" {
				if text, ok := child.(string); ok {
					if strings.EqualFold(text, string(AuthorityModelProposal)) ||
						strings.EqualFold(text, "AUTHORITY_CLASS_MODEL_PROPOSAL") {
						return fmt.Errorf("%w: model proposal elevated: %s", ErrSanitizationFailed, ReasonAuthorityClassInvalid)
					}
					if !strings.EqualFold(text, string(AuthorityMachineObservation)) &&
						!strings.EqualFold(text, "AUTHORITY_CLASS_MACHINE_OBSERVATION") &&
						!strings.EqualFold(text, "2") {
						return fmt.Errorf("%w: non-observation authority %q", ErrSanitizationFailed, text)
					}
				}
			}
			if err := walkJSON(child, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := walkJSON(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

// DigestBundle binds sanitized outcome bundle bytes.
func DigestBundle(bundle []byte) contracts.Digest {
	sum := sha256.Sum256(bundle)
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

// RetainRawTrace documents that raw traces stay outside admission. It validates
// the separation flag and never stores the trace in the admissions ledger.
func RetainRawTrace(record RawTraceRecord) error {
	if record.TraceID == "" || record.Scope == "" || !validDigest(record.Digest) {
		return fmt.Errorf("%w: raw trace record incomplete", ErrInvalidInput)
	}
	if !record.SeparatedFromOutcome {
		return fmt.Errorf("%w: %s", ErrSanitizationFailed, ReasonRawTraceNotSeparated)
	}
	return nil
}

func validDigest(d contracts.Digest) bool {
	if d.Algorithm != "sha256" || len(d.Hex) != 64 {
		return false
	}
	for _, character := range d.Hex {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}
