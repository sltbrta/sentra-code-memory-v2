package contentprivacy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	hardMaxContentBytes = 1 << 20
	hardMaxFindings     = 1024
	hardMaxRetention    = 10 * 365 * 24 * time.Hour
)

type storedRecord struct {
	raw        Input
	projection Projection
	status     Status
	expiresAt  time.Time
	generation uint64
	pending    bool
}

type publishAdmission struct {
	key        string
	input      Input
	status     Status
	kind       string
	classes    []Class
	generation uint64
}

// Guard owns the bounded in-memory lifecycle for one immutable policy version.
// It is concurrency-safe. Production persistence remains the caller's concern.
type Guard struct {
	mu           sync.Mutex
	policy       Policy
	policyDigest string
	detector     Detector
	authorizer   RevealAuthorizer
	now          func() time.Time
	records      map[string]storedRecord
	tombstones   map[string]Tombstone
	receipts     []Receipt
	seq          uint64
	generation   uint64
}

// New returns a guard after canonical policy validation. A nil detector uses
// the local deterministic detector; a nil authorizer makes every reveal deny.
func New(policy Policy, detector Detector, authorizer RevealAuthorizer, clock func() time.Time) (*Guard, error) {
	digest, err := validateAndDigestPolicy(policy)
	if err != nil {
		return nil, ErrInvalid
	}
	if detector == nil {
		detector = LocalDetector{}
	}
	if clock == nil {
		clock = time.Now
	}
	policy = clonePolicy(policy)
	g := &Guard{
		policy: policy, policyDigest: digest, detector: detector,
		authorizer: authorizer, now: clock, records: make(map[string]storedRecord),
		tombstones: make(map[string]Tombstone),
	}
	g.mu.Lock()
	g.seq++
	g.receipts = append(g.receipts, Receipt{
		Seq: g.seq, Kind: ReceiptPolicyInstall, PolicyID: policy.ID,
		PolicyVersion: policy.Version, PolicyDigest: digest, At: clock().UTC(),
	})
	g.mu.Unlock()
	return g, nil
}

// PolicyReceipt returns the immutable installation receipt.
func (g *Guard) PolicyReceipt() Receipt {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneReceipt(g.receipts[0])
}

// Admit is the sole construction boundary for publishable projections. It
// classifies source content and claims, derives cache/index text only from the
// sanitized content, and removes citations that touch a redacted content span.
// A detector error still commits the configured quarantine or tombstone, then
// returns ErrDetector so callers cannot mistake it for clean.
func (g *Guard) Admit(input Input) (Decision, error) {
	decision, _, err := g.admit(input, false)
	return decision, err
}

// preparePublish performs the same fail-closed validation as Admit, but leaves
// a publishable record unreadable and receipt-free until commitPublish. A
// quarantine, tombstone, or detector failure is committed immediately because
// those outcomes never cross the publisher boundary.
func (g *Guard) preparePublish(input Input) (Decision, *publishAdmission, error) {
	return g.admit(input, true)
}

func (g *Guard) admit(input Input, deferPublish bool) (Decision, *publishAdmission, error) {
	scopePolicy, ok := g.policy.Scopes[input.Scope.Kind]
	if !ok || validateInput(input, g.policy.MaxContentBytes) != nil {
		return Decision{}, nil, ErrInvalid
	}
	key := contentKey(input.TenantID, input.Scope, input.ID)
	g.mu.Lock()
	if _, dead := g.tombstones[key]; dead {
		g.mu.Unlock()
		return Decision{}, nil, ErrDenied
	}
	if _, exists := g.records[key]; exists {
		g.mu.Unlock()
		return Decision{}, nil, ErrConflict
	}
	g.mu.Unlock()

	projection, findings, status, detectErr := g.inspect(input, scopePolicy)
	if detectErr != nil {
		decision, err := g.commitDetectorFailure(key, input, scopePolicy)
		return decision, nil, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dead := g.tombstones[key]; dead {
		return Decision{}, nil, ErrDenied
	}
	if _, exists := g.records[key]; exists {
		return Decision{}, nil, ErrConflict
	}
	g.generation++
	now := g.now().UTC()
	kind := ReceiptContentClean
	switch status {
	case StatusClean:
		g.records[key] = storedRecord{
			raw: cloneInput(input), projection: projection, status: status,
			expiresAt: now.Add(scopePolicy.Retention), generation: g.generation,
			pending: deferPublish,
		}
	case StatusRedacted:
		kind = ReceiptContentRedact
		g.records[key] = storedRecord{
			raw: cloneInput(input), projection: projection, status: status,
			expiresAt: now.Add(scopePolicy.Retention), generation: g.generation,
			pending: deferPublish,
		}
	case StatusQuarantined:
		kind = ReceiptContentQuarantine
		g.records[key] = storedRecord{
			raw: cloneInput(input), status: status,
			expiresAt: now.Add(scopePolicy.Retention), generation: g.generation,
		}
	case StatusTombstoned:
		kind = ReceiptContentTombstone
		g.tombstones[key] = Tombstone{
			TenantID: input.TenantID, ContentID: input.ID, ScopeKey: input.Scope.Key(),
			Reason: "policy", At: now,
		}
	default:
		return Decision{}, nil, ErrInvalid
	}
	classes := findingClasses(findings)
	decision := Decision{Status: status, Findings: cloneFindings(findings)}
	if status == StatusClean || status == StatusRedacted {
		copyProjection := cloneProjection(projection)
		decision.Projection = &copyProjection
		if deferPublish {
			return decision, &publishAdmission{
				key: key, input: cloneInput(input), status: status, kind: kind,
				classes: classes, generation: g.generation,
			}, nil
		}
	}
	decision.Receipt = g.appendReceiptLocked(kind, input, status, classes, now)
	return decision, nil, nil
}

func (g *Guard) commitPublish(admission *publishAdmission) (Receipt, error) {
	if admission == nil {
		return Receipt{}, ErrInvalid
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	record, exists := g.records[admission.key]
	if !exists || record.generation != admission.generation || !record.pending {
		if _, dead := g.tombstones[admission.key]; dead {
			return Receipt{}, ErrDenied
		}
		return Receipt{}, ErrConflict
	}
	committedAt := g.now().UTC()
	record.pending = false
	record.expiresAt = committedAt.Add(g.policy.Scopes[admission.input.Scope.Kind].Retention)
	g.records[admission.key] = record
	return g.appendReceiptLocked(admission.kind, admission.input, admission.status, admission.classes, committedAt), nil
}

func (g *Guard) rollbackPublish(admission *publishAdmission) {
	if admission == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	record, exists := g.records[admission.key]
	if exists && record.generation == admission.generation && record.pending {
		delete(g.records, admission.key)
	}
}

// inspect builds every query-facing text surface from detector-checked input.
// Keep this construction centralized: adding caller-provided cache or index
// text would create a path around content inspection.
func (g *Guard) inspect(input Input, scopePolicy ScopePolicy) (Projection, []Finding, Status, error) {
	classes := sortedClasses(scopePolicy.Actions)
	content, contentFindings, err := g.inspectSurface(input.Content, "content", classes, scopePolicy)
	if err != nil {
		return Projection{}, nil, "", err
	}
	findings := append([]Finding(nil), contentFindings...)
	claims := make([]Claim, len(input.Claims))
	for i, claim := range input.Claims {
		text, detected, err := g.inspectSurface(claim.Text, "claim:"+claim.ID, classes, scopePolicy)
		if err != nil {
			return Projection{}, nil, "", err
		}
		findings = append(findings, detected...)
		if len(findings) > g.policy.MaxFindings {
			return Projection{}, nil, "", ErrDetector
		}
		claims[i] = Claim{ID: claim.ID, Text: text}
	}
	sortFindings(findings)
	status := strongestStatus(findings, scopePolicy)
	projection := Projection{
		TenantID: input.TenantID, ID: input.ID, Scope: input.Scope,
		Content: content, IndexText: content, CacheText: content,
		Claims: claims, Blind: input.Blind, PolicyID: g.policy.ID,
		Version: g.policy.Version, PolicyDigest: g.policyDigest,
	}
	projection.Citations = sanitizeCitations(content, input.Citations, contentFindings)
	return projection, findings, status, nil
}

func (g *Guard) inspectSurface(text, surface string, classes []Class, scopePolicy ScopePolicy) (string, []Finding, error) {
	findings, err := detectSafely(g.detector, text, append([]Class(nil), classes...))
	if err != nil {
		return "", nil, ErrDetector
	}
	findings = cloneFindings(findings)
	for i := range findings {
		finding := &findings[i]
		if finding.Surface != "" || finding.Start < 0 || finding.End <= finding.Start || finding.End > len(text) ||
			!utf8Boundary(text, finding.Start) || !utf8Boundary(text, finding.End) {
			return "", nil, ErrDetector
		}
		if _, enabled := scopePolicy.Actions[finding.Class]; !enabled {
			return "", nil, ErrDetector
		}
		finding.Surface = surface
	}
	if len(findings) > g.policy.MaxFindings {
		return "", nil, ErrDetector
	}
	sortFindings(findings)
	redacted := redactBytes(text, findings)
	return redacted, findings, nil
}

func (g *Guard) commitDetectorFailure(key string, input Input, scopePolicy ScopePolicy) (Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dead := g.tombstones[key]; dead {
		return Decision{}, ErrDenied
	}
	if _, exists := g.records[key]; exists {
		return Decision{}, ErrConflict
	}
	now := g.now().UTC()
	g.generation++
	status := StatusQuarantined
	kind := ReceiptDetectorQuarantine
	if scopePolicy.DetectorFailure == ActionTombstone {
		status = StatusTombstoned
		kind = ReceiptDetectorTombstone
		g.tombstones[key] = Tombstone{
			TenantID: input.TenantID, ContentID: input.ID, ScopeKey: input.Scope.Key(),
			Reason: "detector_failure", At: now,
		}
	} else {
		g.records[key] = storedRecord{
			raw: cloneInput(input), status: status,
			expiresAt: now.Add(scopePolicy.Retention), generation: g.generation,
		}
	}
	receipt := g.appendReceiptLocked(kind, input, status, nil, now)
	return Decision{Status: status, Receipt: receipt}, ErrDetector
}

// Projection returns only publishable sanitized content. Expiry is enforced on
// every read even if the caller has not run EnforceRetention.
func (g *Guard) Projection(tenantID string, scope Scope, contentID string) (Projection, error) {
	if !validID(tenantID) || !validID(contentID) || !validScope(scope) {
		return Projection{}, ErrInvalid
	}
	key := contentKey(tenantID, scope, contentID)
	g.mu.Lock()
	defer g.mu.Unlock()
	record, ok := g.records[key]
	if !ok || record.pending || (record.status != StatusClean && record.status != StatusRedacted) {
		return Projection{}, ErrDenied
	}
	now := g.now().UTC()
	if !now.Before(record.expiresAt) {
		g.expireLocked(key, record, now)
		return Projection{}, ErrDenied
	}
	return cloneProjection(record.projection), nil
}

// Reveal returns the original text only after explicit policy opt-in and a
// current external authorization. Authorization is rechecked on every call;
// expiry or a concurrent tombstone wins before the raw copy is returned.
func (g *Guard) Reveal(tenantID string, scope Scope, contentID, principal, reason string) (Revealed, error) {
	if !validID(tenantID) || !validID(contentID) || !validScope(scope) || !validID(principal) || reason == "" || len(reason) > 256 {
		return Revealed{}, ErrInvalid
	}
	scopePolicy, ok := g.policy.Scopes[scope.Kind]
	if !ok || !scopePolicy.AllowReveal || g.authorizer == nil {
		return Revealed{}, ErrDenied
	}
	key := contentKey(tenantID, scope, contentID)
	now := g.now().UTC()
	g.mu.Lock()
	record, exists := g.records[key]
	if !exists || record.pending {
		g.mu.Unlock()
		return Revealed{}, ErrDenied
	}
	if !now.Before(record.expiresAt) {
		g.expireLocked(key, record, now)
		g.mu.Unlock()
		return Revealed{}, ErrDenied
	}
	raw := cloneInput(record.raw)
	generation := record.generation
	g.mu.Unlock()

	request := RevealRequest{
		TenantID: tenantID, ContentID: contentID, Scope: scope,
		Principal: principal, Reason: reason, At: now,
	}
	if err := g.authorizer.AuthorizeReveal(request); err != nil {
		return Revealed{}, ErrDenied
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	current, exists := g.records[key]
	finalNow := g.now().UTC()
	if !exists || current.generation != generation || !finalNow.Before(current.expiresAt) {
		if exists && !finalNow.Before(current.expiresAt) {
			g.expireLocked(key, current, finalNow)
		}
		return Revealed{}, ErrDenied
	}
	receipt := g.appendReceiptLocked(ReceiptAuthorizedReveal, raw, current.status, nil, finalNow)
	return Revealed{
		Content: raw.Content, Claims: cloneClaims(raw.Claims),
		Citations: cloneCitations(raw.Citations), Receipt: receipt,
	}, nil
}

// Tombstone immediately removes all retained content and leaves an
// authoritative non-content marker. Unknown valid ids are also tombstoned.
func (g *Guard) Tombstone(tenantID string, scope Scope, contentID, reason string) (Receipt, error) {
	if !validID(tenantID) || !validID(contentID) || !validScope(scope) || reason == "" || len(reason) > 256 {
		return Receipt{}, ErrInvalid
	}
	key := contentKey(tenantID, scope, contentID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.tombstones[key]; exists {
		return Receipt{}, ErrDenied
	}
	delete(g.records, key)
	now := g.now().UTC()
	g.tombstones[key] = Tombstone{
		TenantID: tenantID, ContentID: contentID, ScopeKey: scope.Key(),
		Reason: reason, At: now,
	}
	input := Input{TenantID: tenantID, ID: contentID, Scope: scope}
	return g.appendReceiptLocked(ReceiptManualTombstone, input, StatusTombstoned, nil, now), nil
}

// EnforceRetention tombstones all expired live/quarantined records in stable
// scoped-key order and returns their receipts.
func (g *Guard) EnforceRetention(at time.Time) ([]Receipt, error) {
	if at.IsZero() {
		return nil, ErrInvalid
	}
	at = at.UTC()
	g.mu.Lock()
	defer g.mu.Unlock()
	trustedNow := g.now().UTC()
	if at.After(trustedNow) {
		return nil, ErrInvalid
	}
	keys := make([]string, 0, len(g.records))
	for key, record := range g.records {
		if !record.pending && !at.Before(record.expiresAt) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	receipts := make([]Receipt, 0, len(keys))
	for _, key := range keys {
		receipts = append(receipts, g.expireLocked(key, g.records[key], trustedNow))
	}
	return receipts, nil
}

func (g *Guard) expireLocked(key string, record storedRecord, at time.Time) Receipt {
	delete(g.records, key)
	raw := record.raw
	g.tombstones[key] = Tombstone{
		TenantID: raw.TenantID, ContentID: raw.ID, ScopeKey: raw.Scope.Key(),
		Reason: "retention", At: at,
	}
	return g.appendReceiptLocked(ReceiptRetentionTombstone, raw, StatusTombstoned, nil, at)
}

// Tombstones returns a stable copy of the authoritative tombstone set.
func (g *Guard) Tombstones() []Tombstone {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Tombstone, 0, len(g.tombstones))
	for _, stone := range g.tombstones {
		out = append(out, stone)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		if out[i].ScopeKey != out[j].ScopeKey {
			return out[i].ScopeKey < out[j].ScopeKey
		}
		return out[i].ContentID < out[j].ContentID
	})
	return out
}

// Receipts returns a deep copy of the append-only receipt log.
func (g *Guard) Receipts() []Receipt {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Receipt, len(g.receipts))
	for i, receipt := range g.receipts {
		out[i] = cloneReceipt(receipt)
	}
	return out
}

func (g *Guard) appendReceiptLocked(kind string, input Input, status Status, classes []Class, at time.Time) Receipt {
	g.seq++
	receipt := Receipt{
		Seq: g.seq, Kind: kind, TenantID: input.TenantID, ContentID: input.ID,
		ScopeKey: input.Scope.Key(), Status: status, Classes: append([]Class(nil), classes...),
		PolicyID: g.policy.ID, PolicyVersion: g.policy.Version,
		PolicyDigest: g.policyDigest, At: at.UTC(),
	}
	g.receipts = append(g.receipts, receipt)
	return cloneReceipt(receipt)
}

func strongestStatus(findings []Finding, policy ScopePolicy) Status {
	status := StatusClean
	for _, finding := range findings {
		switch policy.Actions[finding.Class] {
		case ActionTombstone:
			return StatusTombstoned
		case ActionQuarantine:
			status = StatusQuarantined
		case ActionRedact:
			if status == StatusClean {
				status = StatusRedacted
			}
		}
	}
	return status
}

func redactBytes(text string, findings []Finding) string {
	if len(findings) == 0 {
		return text
	}
	out := []byte(text)
	for _, finding := range findings {
		for i := finding.Start; i < finding.End; i++ {
			out[i] = '*'
		}
	}
	return string(out)
}

// sanitizeCitations keeps only citations whose entire range is outside every
// detected content span. A masked quote at its original sensitive offsets is
// still a citation to redacted material, so overlapping citations are removed
// rather than regenerated from '*' bytes.
func sanitizeCitations(content string, citations []Citation, contentFindings []Finding) []Citation {
	sanitized := make([]Citation, 0, len(citations))
	for _, citation := range citations {
		if overlapsFinding(citation.Start, citation.End, contentFindings) {
			continue
		}
		citation.Quote = content[citation.Start:citation.End]
		sanitized = append(sanitized, citation)
	}
	return sanitized
}

func overlapsFinding(start, end int, findings []Finding) bool {
	for _, finding := range findings {
		if start < finding.End && finding.Start < end {
			return true
		}
	}
	return false
}

func findingClasses(findings []Finding) []Class {
	seen := make(map[Class]struct{})
	for _, finding := range findings {
		seen[finding.Class] = struct{}{}
	}
	out := make([]Class, 0, len(seen))
	for class := range seen {
		out = append(out, class)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedClasses(actions map[Class]Action) []Class {
	out := make([]Class, 0, len(actions))
	for class := range actions {
		out = append(out, class)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func validateAndDigestPolicy(policy Policy) (string, error) {
	if !validID(policy.ID) || !validID(policy.Version) || policy.MaxContentBytes <= 0 || policy.MaxContentBytes > hardMaxContentBytes || policy.MaxFindings <= 0 || policy.MaxFindings > hardMaxFindings || len(policy.Scopes) == 0 || len(policy.Scopes) > 3 {
		return "", ErrInvalid
	}
	type canonicalAction struct {
		Class  Class  `json:"class"`
		Action Action `json:"action"`
	}
	type canonicalScope struct {
		Kind            ScopeKind         `json:"kind"`
		Actions         []canonicalAction `json:"actions"`
		DetectorFailure Action            `json:"detector_failure"`
		RetentionNanos  int64             `json:"retention_nanos"`
		AllowReveal     bool              `json:"allow_reveal"`
	}
	canonical := struct {
		ID              string           `json:"id"`
		Version         string           `json:"version"`
		MaxContentBytes int              `json:"max_content_bytes"`
		MaxFindings     int              `json:"max_findings"`
		Scopes          []canonicalScope `json:"scopes"`
	}{ID: policy.ID, Version: policy.Version, MaxContentBytes: policy.MaxContentBytes, MaxFindings: policy.MaxFindings}
	for kind, scopePolicy := range policy.Scopes {
		if kind != ScopeIndividual && kind != ScopeTeam && kind != ScopeCompany || len(scopePolicy.Actions) == 0 || len(scopePolicy.Actions) > len(localPatterns) || scopePolicy.Retention <= 0 || scopePolicy.Retention > hardMaxRetention || (scopePolicy.DetectorFailure != ActionQuarantine && scopePolicy.DetectorFailure != ActionTombstone) {
			return "", ErrInvalid
		}
		entry := canonicalScope{Kind: kind, DetectorFailure: scopePolicy.DetectorFailure, RetentionNanos: int64(scopePolicy.Retention), AllowReveal: scopePolicy.AllowReveal}
		for _, class := range sortedClasses(scopePolicy.Actions) {
			action := scopePolicy.Actions[class]
			if _, supported := localPatterns[class]; !supported || (action != ActionRedact && action != ActionQuarantine && action != ActionTombstone) {
				return "", ErrInvalid
			}
			entry.Actions = append(entry.Actions, canonicalAction{Class: class, Action: action})
		}
		canonical.Scopes = append(canonical.Scopes, entry)
	}
	sort.Slice(canonical.Scopes, func(i, j int) bool { return canonical.Scopes[i].Kind < canonical.Scopes[j].Kind })
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateInput(input Input, maxBytes int) error {
	if !validID(input.TenantID) || !validID(input.ID) || !validScope(input.Scope) || input.Content == "" || !utf8.ValidString(input.Content) {
		return ErrInvalid
	}
	total := len(input.Content)
	claimIDs := make(map[string]struct{}, len(input.Claims))
	for _, claim := range input.Claims {
		if !validID(claim.ID) || claim.Text == "" || !utf8.ValidString(claim.Text) {
			return ErrInvalid
		}
		if _, duplicate := claimIDs[claim.ID]; duplicate {
			return ErrInvalid
		}
		claimIDs[claim.ID] = struct{}{}
		if len(claim.Text) > maxBytes-total {
			return ErrInvalid
		}
		total += len(claim.Text)
	}
	citationIDs := make(map[string]struct{}, len(input.Citations))
	for _, citation := range input.Citations {
		if !validID(citation.ID) || citation.Start < 0 || citation.End <= citation.Start || citation.End > len(input.Content) || input.Content[citation.Start:citation.End] != citation.Quote {
			return ErrInvalid
		}
		if _, duplicate := citationIDs[citation.ID]; duplicate {
			return ErrInvalid
		}
		citationIDs[citation.ID] = struct{}{}
	}
	if total > maxBytes {
		return ErrInvalid
	}
	return nil
}

func utf8Boundary(text string, offset int) bool {
	return offset == 0 || offset == len(text) || utf8.RuneStart(text[offset])
}

func validScope(scope Scope) bool {
	switch scope.Kind {
	case ScopeIndividual, ScopeTeam:
		return validID(scope.ID)
	case ScopeCompany:
		return scope.ID == ""
	default:
		return false
	}
}

func validID(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "|/\\") && !strings.Contains(value, "..") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func detectSafely(detector Detector, text string, classes []Class) (findings []Finding, err error) {
	defer func() {
		if recover() != nil {
			findings = nil
			err = ErrDetector
		}
	}()
	return detector.Detect(text, classes)
}

func contentKey(tenantID string, scope Scope, contentID string) string {
	return encodeKey(tenantID, scope.Key(), contentID)
}

func encodeKey(parts ...string) string {
	var out strings.Builder
	for _, part := range parts {
		out.WriteString(strconv.Itoa(len(part)))
		out.WriteByte(':')
		out.WriteString(part)
	}
	return out.String()
}

func cloneInput(input Input) Input {
	input.Claims = cloneClaims(input.Claims)
	input.Citations = cloneCitations(input.Citations)
	return input
}

func clonePolicy(policy Policy) Policy {
	cloned := policy
	cloned.Scopes = make(map[ScopeKind]ScopePolicy, len(policy.Scopes))
	for kind, scopePolicy := range policy.Scopes {
		copiedScope := scopePolicy
		copiedScope.Actions = make(map[Class]Action, len(scopePolicy.Actions))
		for class, action := range scopePolicy.Actions {
			copiedScope.Actions[class] = action
		}
		cloned.Scopes[kind] = copiedScope
	}
	return cloned
}

func cloneProjection(projection Projection) Projection {
	projection.Claims = cloneClaims(projection.Claims)
	projection.Citations = cloneCitations(projection.Citations)
	return projection
}

func cloneClaims(claims []Claim) []Claim             { return append([]Claim(nil), claims...) }
func cloneCitations(citations []Citation) []Citation { return append([]Citation(nil), citations...) }
func cloneFindings(findings []Finding) []Finding     { return append([]Finding(nil), findings...) }

func cloneReceipt(receipt Receipt) Receipt {
	receipt.Classes = append([]Class(nil), receipt.Classes...)
	return receipt
}
