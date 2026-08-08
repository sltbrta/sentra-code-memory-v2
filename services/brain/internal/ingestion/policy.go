package ingestion

import (
	"encoding/binary"
	"fmt"
	"hash"
	"path"
	"strings"
)

type ignoreRule struct {
	pattern   string
	directory bool
}

type ignorePolicy struct {
	rules  []ignoreRule
	digest string
}

func parseIgnorePolicy(config Policy, files map[string][]byte, maxPathBytes int) (ignorePolicy, error) {
	if config.Symlinks != RecordWithoutFollow {
		return ignorePolicy{}, fmt.Errorf("symlink mode: %w", ErrUnsupportedPolicy)
	}
	policy := ignorePolicy{}
	hasher := newIdentityHasher("ouroboros.stage03.ignore-policy.v1")
	writeIdentityField(hasher, string(config.Symlinks))
	for _, candidate := range []struct {
		name    string
		enabled bool
	}{
		{name: ".gitignore", enabled: config.UseGitIgnore},
		{name: ".ouroborosignore", enabled: config.UseOuroborosIgnore},
	} {
		writeIdentityField(hasher, fmt.Sprintf("%t", candidate.enabled))
		if !candidate.enabled {
			continue
		}
		rules, err := parseIgnoreFile(candidate.name, files[candidate.name], maxPathBytes)
		if err != nil {
			return ignorePolicy{}, err
		}
		for _, rule := range rules {
			writeIdentityField(hasher, candidate.name)
			writeIdentityField(hasher, rule.pattern)
			writeIdentityField(hasher, fmt.Sprintf("%t", rule.directory))
		}
		policy.rules = append(policy.rules, rules...)
	}
	policy.digest = finishIdentity(hasher)
	return policy, nil
}

func parseIgnoreFile(name string, contents []byte, maxPathBytes int) ([]ignoreRule, error) {
	if len(contents) == 0 {
		return nil, nil
	}
	if strings.IndexByte(string(contents), 0) >= 0 {
		return nil, fmt.Errorf("%s contains NUL: %w", name, ErrUnsupportedPolicy)
	}
	lines := strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n")
	rules := make([]ignoreRule, 0, len(lines))
	for lineNumber, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, err := parseIgnoreRule(line, maxPathBytes)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", name, lineNumber+1, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func parseIgnoreRule(value string, maxPathBytes int) (ignoreRule, error) {
	if strings.HasPrefix(value, "!") || strings.Contains(value, "**") ||
		strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\[]") ||
		strings.TrimSpace(value) != value {
		return ignoreRule{}, ErrUnsupportedPolicy
	}
	directory := strings.HasSuffix(value, "/")
	pattern := strings.TrimSuffix(value, "/")
	if err := validateRepositoryPath(pattern, maxPathBytes); err != nil {
		return ignoreRule{}, fmt.Errorf("ignore path: %w", ErrUnsupportedPolicy)
	}
	for _, segment := range strings.Split(pattern, "/") {
		if _, err := path.Match(segment, segment); err != nil {
			return ignoreRule{}, ErrUnsupportedPolicy
		}
	}
	return ignoreRule{pattern: pattern, directory: directory}, nil
}

func (policy ignorePolicy) ignores(name string) bool {
	segments := strings.Split(name, "/")
	for _, rule := range policy.rules {
		ruleSegments := strings.Split(rule.pattern, "/")
		if rule.directory && len(segments) <= len(ruleSegments) {
			continue
		}
		if !rule.directory && len(segments) != len(ruleSegments) {
			continue
		}
		matched := true
		for index, pattern := range ruleSegments {
			segmentMatch, err := path.Match(pattern, segments[index])
			if err != nil || !segmentMatch {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func writeIdentityField(hasher hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write([]byte(value))
}
