package contentprivacy

import (
	"regexp"
	"sort"
	"strings"
)

// LocalDetector is the dependency-free deterministic detector. Its patterns
// are intentionally narrow and versioned with this package; policy selects the
// classes to run. It is not a claim of universal DLP coverage.
type LocalDetector struct{}

var localPatterns = map[Class]*regexp.Regexp{
	ClassEmail:      regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,63}`),
	ClassPhone:      regexp.MustCompile(`(?:\+?[0-9][0-9 .()\-]{7,}[0-9])`),
	ClassSSN:        regexp.MustCompile(`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`),
	ClassCreditCard: regexp.MustCompile(`\b(?:[0-9][ -]?){12,18}[0-9]\b`),
	// The alternation required an underscore after sk/api/key, so every
	// vendor prefix in common use went undetected: OpenAI sk-, GitHub ghp_ and
	// friends, Slack xox*-, Google AIza, and a bare 40-character AWS secret.
	// A redactor that misses the formats secrets actually take is a redactor in
	// name only.
	ClassAPIKey: regexp.MustCompile(`\b(?:` +
		`AKIA[0-9A-Z]{16}` + // AWS access key id
		`|(?:sk|api|key)_[A-Za-z0-9_\-]{12,}` + // generic underscore form, e.g. sk_live_...
		`|sk-(?:proj-|ant-|or-)?[A-Za-z0-9_\-]{16,}` + // OpenAI and Anthropic
		`|gh[pousr]_[A-Za-z0-9]{16,}` + // GitHub personal/oauth/user/server/refresh
		`|github_pat_[A-Za-z0-9_]{20,}` + // GitHub fine-grained
		`|xox[baprs]-[A-Za-z0-9\-]{10,}` + // Slack
		`|AIza[0-9A-Za-z_\-]{30,}` + // Google API key (length varies by product)
		`|glpat-[A-Za-z0-9_\-]{16,}` + // GitLab
		`|(?:r8_|hf_)[A-Za-z0-9]{16,}` + // Replicate, Hugging Face
		`)\b`),
	ClassBearerToken: regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/\-]{12,}=*`),
	// Two rules. The first matches a complete key; the second matches a BEGIN
	// marker with no END, redacting to the end of the input. Requiring the END
	// marker meant a key truncated by chunking or by MaxContentBytes matched
	// nothing at all and its body was emitted verbatim -- the failure mode is
	// exactly the one that matters.
	ClassPrivateKey: regexp.MustCompile(`(?s)-----BEGIN[ \t]+[A-Z0-9 \t]*PRIVATE KEY-----` +
		`(?:.*?-----END[ \t]+[A-Z0-9 \t]*PRIVATE KEY-----|.*)`),
	ClassPasswordAssignment: regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret)[ \t]*[:=][ \t]*[^\s,;]+`),
}

// Detect returns findings ordered by byte start, longest match, then class.
func (LocalDetector) Detect(text string, classes []Class) ([]Finding, error) {
	ordered := append([]Class(nil), classes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	findings := make([]Finding, 0)
	for i, class := range ordered {
		if i > 0 && ordered[i-1] == class {
			continue
		}
		re, ok := localPatterns[class]
		if !ok {
			return nil, ErrInvalid
		}
		for _, span := range re.FindAllStringIndex(text, -1) {
			if class == ClassCreditCard && !validCard(text[span[0]:span[1]]) {
				continue
			}
			findings = append(findings, Finding{Class: class, Start: span[0], End: span[1]})
		}
	}
	sortFindings(findings)
	return findings, nil
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Surface != findings[j].Surface {
			return findings[i].Surface < findings[j].Surface
		}
		if findings[i].Start != findings[j].Start {
			return findings[i].Start < findings[j].Start
		}
		if findings[i].End != findings[j].End {
			return findings[i].End > findings[j].End
		}
		return findings[i].Class < findings[j].Class
	})
}

func validCard(value string) bool {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	parity := len(digits) % 2
	for i := range digits {
		n := int(digits[i] - '0')
		if i%2 == parity {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}
