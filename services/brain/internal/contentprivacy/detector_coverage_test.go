package contentprivacy_test

import (
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

// The API-key alternation required an underscore after sk/api/key, so every
// vendor prefix in common use went undetected and was published unredacted. The
// private-key rule required a matching END marker, so a key truncated by
// chunking matched nothing at all -- the case that matters most.

// The fixtures below are synthetic, but a literal vendor prefix in checked-in
// source is what secret scanners are built to find, and a server-side scanner
// does not read this repository's gitleaks allowlist. Each fixture is therefore
// assembled from a split prefix at run time: the detector sees the identical
// bytes, and no scannable literal exists in the file.
func TestDetectorFindsCommonVendorKeyFormats(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		rest   string
	}{
		{"AWS access key id", "AKI" + "A", "IOSFODNN7EXAMPLE"},
		{"OpenAI", "s" + "k-", "abcdefghijklmnopqrstuvwxyz012345"},
		{"OpenAI project", "s" + "k-proj-", "abcdefghijklmnopqrstuvwxyz012345"},
		{"Anthropic", "s" + "k-ant-", "abcdefghijklmnopqrstuvwxyz012345"},
		{"GitHub PAT", "gh" + "p_", "abcdefghijklmnopqrstuvwxyz0123456789"},
		{"GitHub oauth", "gh" + "o_", "abcdefghijklmnopqrstuvwxyz0123456789"},
		{"GitHub fine grain", "github" + "_pat_", "11ABCDEFG0abcdefghijklmnopqrstuvwxyz"},
		{"Slack bot", "xox" + "b-", "1234567890-abcdefghijklmno"},
		{"Google API key", "AIza" + "SyA", "1234567890abcdefghijklmnopqrstuvw"},
		{"GitLab PAT", "glp" + "at-", "abcdefghijklmnopqrst"},
		{"Hugging Face", "h" + "f_", "abcdefghijklmnopqrstuvwxyz"},
		{"generic sk_", "s" + "k_live_", "abcdefghijklmnop"},
	}
	detector := contentprivacy.LocalDetector{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret := tc.prefix + tc.rest
			text := "config value = " + secret + " end"
			findings, err := detector.Detect(text, []contentprivacy.Class{contentprivacy.ClassAPIKey})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if len(findings) == 0 {
				t.Fatalf("%s (%q) was not detected and would be published unredacted", tc.name, secret)
			}
		})
	}
}

func TestDetectorFindsATruncatedPrivateKey(t *testing.T) {
	// No END marker: what chunking or a byte cap leaves behind.
	truncated := "-----BEGIN " + "RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIEowIBAAKCAQEAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", 4)

	detector := contentprivacy.LocalDetector{}
	findings, err := detector.Detect(truncated, []contentprivacy.Class{contentprivacy.ClassPrivateKey})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("a private key without its END marker was not detected: its body would be emitted verbatim")
	}
}

func TestDetectorStillFindsACompletePrivateKey(t *testing.T) {
	complete := "-----BEGIN " + "PRIVATE KEY-----\nMIIEvQIBADAN\n-----END " + "PRIVATE KEY-----"
	detector := contentprivacy.LocalDetector{}
	findings, err := detector.Detect(complete, []contentprivacy.Class{contentprivacy.ClassPrivateKey})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding for a complete key, got %d", len(findings))
	}
}

// TestDetectorDoesNotFlagOrdinaryProse keeps the widened patterns from turning
// every document into a redaction.
func TestDetectorDoesNotFlagOrdinaryProse(t *testing.T) {
	detector := contentprivacy.LocalDetector{}
	for _, text := range []string{
		"the skeleton key opens every door",
		"see the api documentation for details",
		"github_pattern matching is hard",
		"a normal sentence with no secrets in it",
	} {
		findings, err := detector.Detect(text, []contentprivacy.Class{contentprivacy.ClassAPIKey})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("ordinary prose %q flagged as a secret: %+v", text, findings)
		}
	}
}
