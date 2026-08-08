package contentprivacy_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

var testNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func basePolicy() contentprivacy.Policy {
	return contentprivacy.Policy{
		ID: "privacy-default", Version: "v1.2.0",
		MaxContentBytes: 4096, MaxFindings: 32,
		Scopes: map[contentprivacy.ScopeKind]contentprivacy.ScopePolicy{
			contentprivacy.ScopeIndividual: {
				Actions: map[contentprivacy.Class]contentprivacy.Action{
					contentprivacy.ClassEmail: contentprivacy.ActionRedact,
				},
				DetectorFailure: contentprivacy.ActionQuarantine,
				Retention:       24 * time.Hour, AllowReveal: true,
			},
			contentprivacy.ScopeTeam: {
				Actions: map[contentprivacy.Class]contentprivacy.Action{
					contentprivacy.ClassAPIKey: contentprivacy.ActionQuarantine,
				},
				DetectorFailure: contentprivacy.ActionQuarantine,
				Retention:       12 * time.Hour, AllowReveal: true,
			},
			contentprivacy.ScopeCompany: {
				Actions: map[contentprivacy.Class]contentprivacy.Action{
					contentprivacy.ClassSSN: contentprivacy.ActionTombstone,
				},
				DetectorFailure: contentprivacy.ActionTombstone,
				Retention:       6 * time.Hour,
			},
		},
	}
}

func newGuard(t *testing.T, policy contentprivacy.Policy, detector contentprivacy.Detector, authorizer contentprivacy.RevealAuthorizer, now *time.Time) *contentprivacy.Guard {
	t.Helper()
	g, err := contentprivacy.New(policy, detector, authorizer, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestIndividualTeamCompanyPolicyDispositions(t *testing.T) {
	now := testNow
	g := newGuard(t, basePolicy(), nil, nil, &now)
	tests := []struct {
		name    string
		input   contentprivacy.Input
		status  contentprivacy.Status
		hasView bool
		kind    string
	}{
		{
			name:   "individual redact",
			input:  contentprivacy.Input{TenantID: "acme", ID: "personal-1", Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}, Content: "mail alice@example.com"},
			status: contentprivacy.StatusRedacted, hasView: true, kind: contentprivacy.ReceiptContentRedact,
		},
		{
			name:   "team quarantine",
			input:  contentprivacy.Input{TenantID: "acme", ID: "team-1", Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeTeam, ID: "eng"}, Content: "token sk_1234567890abcd"},
			status: contentprivacy.StatusQuarantined, kind: contentprivacy.ReceiptContentQuarantine,
		},
		{
			name:   "company tombstone",
			input:  contentprivacy.Input{TenantID: "acme", ID: "company-1", Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeCompany}, Content: "employee 123-45-6789"},
			status: contentprivacy.StatusTombstoned, kind: contentprivacy.ReceiptContentTombstone,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := g.Admit(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Status != test.status || (decision.Projection != nil) != test.hasView || decision.Receipt.Kind != test.kind {
				t.Fatalf("decision = %+v", decision)
			}
			if decision.Receipt.PolicyID != "privacy-default" || decision.Receipt.PolicyVersion != "v1.2.0" || decision.Receipt.PolicyDigest == "" {
				t.Fatalf("missing policy identity: %+v", decision.Receipt)
			}
		})
	}
	if _, err := g.Projection("acme", contentprivacy.Scope{Kind: contentprivacy.ScopeTeam, ID: "eng"}, "team-1"); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("quarantine projection = %v", err)
	}
	if _, err := g.Projection("acme", contentprivacy.Scope{Kind: contentprivacy.ScopeCompany}, "company-1"); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("tombstone projection = %v", err)
	}
}

func TestProjectionSanitizesDerivedTextAndDropsCitationToRedactedSpan(t *testing.T) {
	now := testNow
	policy := basePolicy()
	individual := policy.Scopes[contentprivacy.ScopeIndividual]
	individual.Actions[contentprivacy.ClassAPIKey] = contentprivacy.ActionRedact
	policy.Scopes[contentprivacy.ScopeIndividual] = individual
	g := newGuard(t, policy, nil, nil, &now)

	content := "Owner alice@example.com uses sk_1234567890abcd today."
	overlappingStart := strings.Index(content, "Owner")
	overlappingEnd := strings.Index(content, " today")
	safeStart := strings.Index(content, "today")
	safeEnd := safeStart + len("today")
	decision, err := g.Admit(contentprivacy.Input{
		TenantID: "acme", ID: "p-1",
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content: content,
		Claims:  []contentprivacy.Claim{{ID: "claim-1", Text: "Reach alice@example.com with sk_1234567890abcd"}},
		Citations: []contentprivacy.Citation{
			{ID: "cite-overlap", Start: overlappingStart, End: overlappingEnd, Quote: content[overlappingStart:overlappingEnd]},
			{ID: "cite-safe", Start: safeStart, End: safeEnd, Quote: content[safeStart:safeEnd]},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := decision.Projection
	if projection == nil {
		t.Fatal("redaction must publish a projection")
	}
	for name, value := range map[string]string{
		"content": projection.Content, "index": projection.IndexText,
		"cache": projection.CacheText, "claim": projection.Claims[0].Text,
	} {
		if strings.Contains(value, "alice@example.com") || strings.Contains(value, "sk_1234567890abcd") {
			t.Fatalf("%s leaked configured content: %q", name, value)
		}
	}
	if len(projection.Content) != len(content) {
		t.Fatalf("redaction moved byte offsets: %d != %d", len(projection.Content), len(content))
	}
	if projection.IndexText != projection.Content || projection.CacheText != projection.Content {
		t.Fatalf("derived output bypassed sanitized content: content=%q index=%q cache=%q", projection.Content, projection.IndexText, projection.CacheText)
	}
	if len(projection.Citations) != 1 {
		t.Fatalf("citations to redacted spans must be absent: %+v", projection.Citations)
	}
	citation := projection.Citations[0]
	if citation.ID != "cite-safe" || citation.Start != safeStart || citation.End != safeEnd || citation.Quote != "today" || citation.Quote != projection.Content[safeStart:safeEnd] {
		t.Fatalf("safe citation changed: %+v", citation)
	}
	for _, finding := range decision.Findings {
		if finding.Surface == "content" && citation.Start < finding.End && finding.Start < citation.End {
			t.Fatalf("citation overlaps redacted finding: citation=%+v finding=%+v", citation, finding)
		}
	}
	loaded, err := g.Projection("acme", projection.Scope, projection.ID)
	if err != nil || !reflect.DeepEqual(loaded, *projection) {
		t.Fatalf("stored projection = %+v, %v", loaded, err)
	}
	loaded.Claims[0].Text = "mutated"
	again, _ := g.Projection("acme", projection.Scope, projection.ID)
	if again.Claims[0].Text == "mutated" {
		t.Fatal("projection returned aliased claim storage")
	}
}

type failingDetector struct{}

func (failingDetector) Detect(string, []contentprivacy.Class) ([]contentprivacy.Finding, error) {
	return nil, errors.New("backend unavailable with sensitive detail")
}

type panickingDetector struct{}

func (panickingDetector) Detect(string, []contentprivacy.Class) ([]contentprivacy.Finding, error) {
	panic("detector exposed raw payload")
}

type floodingDetector struct{}

func (floodingDetector) Detect(text string, _ []contentprivacy.Class) ([]contentprivacy.Finding, error) {
	findings := make([]contentprivacy.Finding, 0, len(text))
	for i := range text {
		findings = append(findings, contentprivacy.Finding{Class: contentprivacy.ClassEmail, Start: i, End: i + 1})
	}
	return findings, nil
}

type fixedFindingDetector struct {
	finding contentprivacy.Finding
}

func (d fixedFindingDetector) Detect(string, []contentprivacy.Class) ([]contentprivacy.Finding, error) {
	return []contentprivacy.Finding{d.finding}, nil
}

func TestDetectorFailureCommitsConfiguredFailClosedDisposition(t *testing.T) {
	now := testNow
	g := newGuard(t, basePolicy(), failingDetector{}, nil, &now)
	input := contentprivacy.Input{
		TenantID: "acme", ID: "failure-1",
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content: "unknown sensitivity",
	}
	decision, err := g.Admit(input)
	if !errors.Is(err, contentprivacy.ErrDetector) || decision.Status != contentprivacy.StatusQuarantined || decision.Projection != nil || decision.Receipt.Kind != contentprivacy.ReceiptDetectorQuarantine {
		t.Fatalf("failure decision = %+v, %v", decision, err)
	}
	if strings.Contains(err.Error(), "backend") {
		t.Fatalf("detector detail escaped: %v", err)
	}
	if _, err := g.Projection("acme", input.Scope, input.ID); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("detector failure projection = %v", err)
	}

	company := contentprivacy.Input{TenantID: "acme", ID: "failure-2", Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeCompany}, Content: "unknown"}
	decision, err = g.Admit(company)
	if !errors.Is(err, contentprivacy.ErrDetector) || decision.Status != contentprivacy.StatusTombstoned || decision.Receipt.Kind != contentprivacy.ReceiptDetectorTombstone {
		t.Fatalf("company failure decision = %+v, %v", decision, err)
	}
}

func TestDetectorPanicAndMalformedCitationFailClosed(t *testing.T) {
	now := testNow
	g := newGuard(t, basePolicy(), panickingDetector{}, nil, &now)
	input := contentprivacy.Input{
		TenantID: "acme", ID: "panic-1",
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content: "possibly sensitive",
	}
	decision, err := g.Admit(input)
	if !errors.Is(err, contentprivacy.ErrDetector) || decision.Status != contentprivacy.StatusQuarantined {
		t.Fatalf("panic did not fail closed: %+v, %v", decision, err)
	}

	g = newGuard(t, basePolicy(), nil, nil, &now)
	content := "alice@example.com"
	input = contentprivacy.Input{
		TenantID: "acme", ID: "bad-citation",
		Scope:     contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content:   content,
		Citations: []contentprivacy.Citation{{ID: "cite-1", Start: 0, End: len(content), Quote: "different raw quote"}},
	}
	if decision, err := g.Admit(input); !errors.Is(err, contentprivacy.ErrInvalid) || decision.Projection != nil {
		t.Fatalf("mismatched citation admitted: %+v, %v", decision, err)
	}
}

func TestFindingAndContentBoundsNeverPublish(t *testing.T) {
	now := testNow
	policy := basePolicy()
	policy.MaxFindings = 2
	g := newGuard(t, policy, floodingDetector{}, nil, &now)
	input := contentprivacy.Input{
		TenantID: "acme", ID: "finding-bound",
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content: "abcd",
	}
	decision, err := g.Admit(input)
	if !errors.Is(err, contentprivacy.ErrDetector) || decision.Status != contentprivacy.StatusQuarantined || decision.Projection != nil {
		t.Fatalf("finding bound did not fail closed: %+v, %v", decision, err)
	}

	policy = basePolicy()
	policy.MaxContentBytes = 4
	g = newGuard(t, policy, nil, nil, &now)
	input.ID = "content-bound"
	input.Content = "abcde"
	if decision, err := g.Admit(input); !errors.Is(err, contentprivacy.ErrInvalid) || decision.Projection != nil {
		t.Fatalf("content bound published: %+v, %v", decision, err)
	}
}

func TestAuthorizedRevealRetentionAndTombstone(t *testing.T) {
	now := testNow
	policy := basePolicy()
	individual := policy.Scopes[contentprivacy.ScopeIndividual]
	individual.Retention = time.Hour
	policy.Scopes[contentprivacy.ScopeIndividual] = individual
	authorizer := contentprivacy.RevealAuthorizerFunc(func(request contentprivacy.RevealRequest) error {
		if request.Principal != "privacy-admin" || request.Reason != "incident-42" || request.Scope.Kind != contentprivacy.ScopeIndividual {
			return contentprivacy.ErrDenied
		}
		return nil
	})
	g := newGuard(t, policy, nil, authorizer, &now)
	scope := contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}
	input := contentprivacy.Input{
		TenantID: "acme", ID: "reveal-1", Scope: scope, Content: "alice@example.com",
		Claims: []contentprivacy.Claim{{ID: "claim-1", Text: "contact alice@example.com"}},
		Citations: []contentprivacy.Citation{{
			ID: "cite-1", Start: 0, End: len("alice@example.com"), Quote: "alice@example.com",
		}},
	}
	if _, err := g.Admit(input); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Reveal("acme", scope, input.ID, "bob", "incident-42"); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("unauthorized reveal = %v", err)
	}
	revealed, err := g.Reveal("acme", scope, input.ID, "privacy-admin", "incident-42")
	if err != nil || revealed.Content != input.Content || !reflect.DeepEqual(revealed.Claims, input.Claims) || !reflect.DeepEqual(revealed.Citations, input.Citations) || revealed.Receipt.Kind != contentprivacy.ReceiptAuthorizedReveal {
		t.Fatalf("authorized reveal = %+v, %v", revealed, err)
	}
	now = now.Add(time.Hour)
	if _, err := g.Projection("acme", scope, input.ID); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("expired projection = %v", err)
	}
	if _, err := g.Reveal("acme", scope, input.ID, "privacy-admin", "incident-42"); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("expired reveal = %v", err)
	}
	stones := g.Tombstones()
	if len(stones) != 1 || stones[0].Reason != "retention" {
		t.Fatalf("retention tombstones = %+v", stones)
	}
	if _, err := g.Admit(input); !errors.Is(err, contentprivacy.ErrDenied) {
		t.Fatalf("retention tombstone allowed resurrection: %v", err)
	}

	unknown := contentprivacy.Scope{Kind: contentprivacy.ScopeTeam, ID: "eng"}
	receipt, err := g.Tombstone("acme", unknown, "never-seen", "deletion_request")
	if err != nil || receipt.Kind != contentprivacy.ReceiptManualTombstone {
		t.Fatalf("manual tombstone = %+v, %v", receipt, err)
	}
}

func TestRetentionSweepRejectsFutureClockAndUsesTrustedTombstoneTime(t *testing.T) {
	now := testNow
	policy := basePolicy()
	individual := policy.Scopes[contentprivacy.ScopeIndividual]
	individual.Retention = time.Hour
	policy.Scopes[contentprivacy.ScopeIndividual] = individual
	g := newGuard(t, policy, nil, nil, &now)
	scope := contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}
	input := contentprivacy.Input{TenantID: "acme", ID: "retention-clock", Scope: scope, Content: "ordinary text"}
	if _, err := g.Admit(input); err != nil {
		t.Fatal(err)
	}

	if receipts, err := g.EnforceRetention(now.Add(time.Second)); !errors.Is(err, contentprivacy.ErrInvalid) || receipts != nil {
		t.Fatalf("future sweep = %+v, %v", receipts, err)
	}
	if _, err := g.Projection(input.TenantID, scope, input.ID); err != nil {
		t.Fatalf("future sweep changed admission: %v", err)
	}
	if stones := g.Tombstones(); len(stones) != 0 {
		t.Fatalf("future sweep created tombstone: %+v", stones)
	}

	expiresAt := now.Add(time.Hour)
	now = now.Add(2 * time.Hour)
	receipts, err := g.EnforceRetention(expiresAt)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("valid sweep = %+v, %v", receipts, err)
	}
	if !receipts[0].At.Equal(now) || receipts[0].Kind != contentprivacy.ReceiptRetentionTombstone {
		t.Fatalf("retention receipt did not use trusted time: %+v, want %v", receipts[0], now)
	}
	stones := g.Tombstones()
	if len(stones) != 1 || !stones[0].At.Equal(now) || stones[0].Reason != "retention" {
		t.Fatalf("retention tombstone did not use trusted time: %+v, want %v", stones, now)
	}
}

func TestPolicyReceiptIsCanonicalAndPolicyCopyIsImmutable(t *testing.T) {
	now := testNow
	p1 := basePolicy()
	p2 := basePolicy()
	// Rebuild one actions map in a different insertion order.
	p2.Scopes[contentprivacy.ScopeIndividual] = contentprivacy.ScopePolicy{
		Actions: map[contentprivacy.Class]contentprivacy.Action{
			contentprivacy.ClassEmail: contentprivacy.ActionRedact,
		},
		DetectorFailure: contentprivacy.ActionQuarantine,
		Retention:       24 * time.Hour, AllowReveal: true,
	}
	g1 := newGuard(t, p1, nil, nil, &now)
	g2 := newGuard(t, p2, nil, nil, &now)
	r1, r2 := g1.PolicyReceipt(), g2.PolicyReceipt()
	if r1.PolicyDigest == "" || r1.PolicyDigest != r2.PolicyDigest || r1.Kind != contentprivacy.ReceiptPolicyInstall || r1.Seq != 1 {
		t.Fatalf("policy receipts = %+v / %+v", r1, r2)
	}
	// Mutating caller-owned maps after construction cannot change enforcement.
	p1.Scopes[contentprivacy.ScopeIndividual].Actions[contentprivacy.ClassEmail] = contentprivacy.ActionTombstone
	decision, err := g1.Admit(contentprivacy.Input{TenantID: "acme", ID: "copy-1", Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}, Content: "alice@example.com"})
	if err != nil || decision.Status != contentprivacy.StatusRedacted {
		t.Fatalf("caller mutated installed policy: %+v, %v", decision, err)
	}
}

func TestBlindNoGoldBoundaryAndMissingScopePolicyFailClosed(t *testing.T) {
	now := testNow
	policy := basePolicy()
	delete(policy.Scopes, contentprivacy.ScopeCompany)
	g := newGuard(t, policy, nil, nil, &now)
	decision, err := g.Admit(contentprivacy.Input{
		TenantID: "acme", ID: "blind-1", Blind: true,
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content: "ordinary text",
	})
	if err != nil || decision.Projection == nil || !decision.Projection.Blind {
		t.Fatalf("blind metadata changed: %+v, %v", decision, err)
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(contentprivacy.Input{}), reflect.TypeOf(contentprivacy.Projection{})} {
		if _, exists := typ.FieldByName("Gold"); exists {
			t.Fatalf("runtime privacy boundary exposes benchmark gold on %s", typ.Name())
		}
	}
	company := contentprivacy.Input{TenantID: "acme", ID: "blind-2", Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeCompany}, Content: "ordinary text"}
	if decision, err := g.Admit(company); !errors.Is(err, contentprivacy.ErrInvalid) || decision.Projection != nil {
		t.Fatalf("missing scope policy did not fail closed: %+v, %v", decision, err)
	}
}

func TestConfiguredLocalPIIAndSecretClasses(t *testing.T) {
	tests := []struct {
		class contentprivacy.Class
		text  string
	}{
		{contentprivacy.ClassEmail, "alice@example.com"},
		{contentprivacy.ClassPhone, "+1 (415) 555-0123"},
		{contentprivacy.ClassSSN, "123-45-6789"},
		{contentprivacy.ClassCreditCard, "4111 1111 1111 1111"},
		{contentprivacy.ClassAPIKey, "sk_" + "1234567890abcd"},
		{contentprivacy.ClassBearerToken, "Bearer abcdefghijklmnop"},
		{contentprivacy.ClassPrivateKey, "-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----"},
		{contentprivacy.ClassPasswordAssignment, "password=hunter2"},
	}
	detector := contentprivacy.LocalDetector{}
	for _, test := range tests {
		t.Run(string(test.class), func(t *testing.T) {
			findings, err := detector.Detect(test.text, []contentprivacy.Class{test.class})
			if err != nil || len(findings) != 1 || findings[0].Class != test.class {
				t.Fatalf("findings = %+v, %v", findings, err)
			}
		})
	}
}
