package query

import (
	"sort"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
)

// tokenCutset is the sentence punctuation stripped from whitespace-delimited
// query tokens before exact path or definition matching. Interior path
// characters are never touched.
const tokenCutset = "\"'`()[]{}<>.,;:!?"

// candidate is one projection file selected as potential evidence, with its
// definition spellings and its coverage disposition.
type candidate struct {
	path        string
	definitions []string
	degraded    bool
}

// tokenizeQuery splits one question into trimmed, case-preserved tokens.
// Exact path matching is case-sensitive; definition matching lowercases both
// sides at comparison time.
func tokenizeQuery(text string) []string {
	raw := strings.Fields(text)
	tokens := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, token := range raw {
		token = strings.Trim(token, tokenCutset)
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		tokens = append(tokens, token)
	}
	return tokens
}

// selectCandidates chooses the bounded, path-ordered candidate set for one
// pinned snapshot. Exact path mentions select the named files directly and
// scope the query; only when no path is mentioned does corpus-wide definition
// matching apply. Lexically degraded files are selectable by exact path (the
// query named them) but never by definition terms, because a degraded lane
// retains no syntax-aware definitions.
func selectCandidates(snapshot Snapshot, terms []string, maxCandidates int) []candidate {
	if snapshot.Projection.State != ProjectionReady || snapshot.Projection.Index == nil {
		return nil
	}
	files := make(map[string]codeindex.FileProjection, len(snapshot.Projection.Index.Files))
	for _, file := range snapshot.Projection.Index.Files {
		files[file.Path] = file
	}
	var selected []codeindex.FileProjection
	mentioned := mentionedPaths(files, terms)
	if len(mentioned) > 0 {
		for _, path := range mentioned {
			selected = append(selected, files[path])
		}
	} else {
		lowered := loweredTerms(terms)
		for _, file := range snapshot.Projection.Index.Files {
			if file.Coverage == codeindex.CoverageLexicalDegraded {
				continue
			}
			if matchesDefinition(file, lowered) {
				selected = append(selected, file)
			}
		}
	}
	if len(selected) > maxCandidates {
		selected = selected[:maxCandidates]
	}
	candidates := make([]candidate, 0, len(selected))
	for _, file := range selected {
		candidates = append(candidates, candidate{
			path:        file.Path,
			definitions: definitionTexts(file),
			degraded:    file.Coverage == codeindex.CoverageLexicalDegraded,
		})
	}
	return candidates
}

// selectCandidatesMulti runs selectCandidates for each multiQueryVariants
// token set, fuses the ranked path lists with classic RRF, and rebuilds the
// candidate slice from the fused paths. Exact path mentions on the original
// question still scope the query: path-scoped originals are never widened by
// paraphrase variants (preserves Stage 04 path-mention and fixture behavior).
// selectCandidates itself is unchanged for direct callers/tests.
func selectCandidatesMulti(snapshot Snapshot, question string, maxCandidates int) []candidate {
	if snapshot.Projection.State != ProjectionReady || snapshot.Projection.Index == nil {
		return nil
	}
	variants := multiQueryVariants(question)
	if len(variants) == 0 {
		return nil
	}
	origTerms := tokenizeQuery(variants[0])
	// Path mentions scope the query; multi-query must not open corpus-wide
	// definition matching when the caller named repository paths.
	if hasPathMention(snapshot, origTerms) {
		return selectCandidates(snapshot, origTerms, maxCandidates)
	}

	byPath := make(map[string]candidate)
	rankedLists := make([][]string, 0, len(variants))
	for _, variant := range variants {
		terms := tokenizeQuery(variant)
		cands := selectCandidates(snapshot, terms, maxCandidates)
		if len(cands) == 0 {
			continue
		}
		paths := make([]string, 0, len(cands))
		for _, c := range cands {
			paths = append(paths, c.path)
			if _, exists := byPath[c.path]; !exists {
				byPath[c.path] = c
			}
		}
		rankedLists = append(rankedLists, paths)
	}
	if len(rankedLists) == 0 {
		return nil
	}
	fused := rrfFuse(rankedLists, defaultRRFK, maxCandidates)
	out := make([]candidate, 0, len(fused))
	for _, path := range fused {
		if c, ok := byPath[path]; ok {
			out = append(out, c)
		}
	}
	return out
}

// hasPathMention reports whether any term is an indexed repository-relative path.
func hasPathMention(snapshot Snapshot, terms []string) bool {
	if snapshot.Projection.Index == nil {
		return false
	}
	files := make(map[string]codeindex.FileProjection, len(snapshot.Projection.Index.Files))
	for _, file := range snapshot.Projection.Index.Files {
		files[file.Path] = file
	}
	return len(mentionedPaths(files, terms)) > 0
}

// mentionedPaths returns the sorted subset of tokens exactly equal to an
// indexed repository-relative path.
func mentionedPaths(files map[string]codeindex.FileProjection, terms []string) []string {
	var mentioned []string
	for _, term := range terms {
		if _, exists := files[term]; exists {
			mentioned = append(mentioned, term)
		}
	}
	sort.Strings(mentioned)
	return mentioned
}

// unindexedMentions returns the sorted canonical-but-unindexed path mentions:
// tokens naming a manifest revision the projection does not index, such as an
// unindexed text file or a record-without-follow symlink.
func unindexedMentions(snapshot Snapshot, terms []string) []string {
	indexed := make(map[string]bool)
	if snapshot.Projection.State == ProjectionReady && snapshot.Projection.Index != nil {
		for _, file := range snapshot.Projection.Index.Files {
			indexed[file.Path] = true
		}
	}
	termSet := make(map[string]bool, len(terms))
	for _, term := range terms {
		termSet[term] = true
	}
	var mentioned []string
	for _, revision := range snapshot.Revisions {
		if indexed[revision.Path] || !termSet[revision.Path] {
			continue
		}
		mentioned = append(mentioned, revision.Path)
	}
	sort.Strings(mentioned)
	return mentioned
}

func loweredTerms(terms []string) map[string]bool {
	lowered := make(map[string]bool, len(terms))
	for _, term := range terms {
		lowered[strings.ToLower(term)] = true
	}
	return lowered
}

func matchesDefinition(file codeindex.FileProjection, lowered map[string]bool) bool {
	for _, occurrence := range file.Occurrences {
		if occurrence.Kind == codeindex.KindDefinition && lowered[strings.ToLower(occurrence.Text)] {
			return true
		}
	}
	return false
}

func definitionTexts(file codeindex.FileProjection) []string {
	var definitions []string
	for _, occurrence := range file.Occurrences {
		if occurrence.Kind == codeindex.KindDefinition {
			definitions = append(definitions, occurrence.Text)
		}
	}
	return definitions
}

// selectDefinition picks the occurrence a candidate answers from: the first
// syntax-aware definition whose spelling case-insensitively matches a query
// term, else the file's first definition in position order. It returns false
// when the file holds no syntax-aware definition.
func selectDefinition(file codeindex.FileProjection, terms []string) (codeindex.Occurrence, bool) {
	lowered := loweredTerms(terms)
	for _, occurrence := range file.Occurrences {
		if occurrence.Kind == codeindex.KindDefinition && lowered[strings.ToLower(occurrence.Text)] {
			return occurrence, true
		}
	}
	for _, occurrence := range file.Occurrences {
		if occurrence.Kind == codeindex.KindDefinition {
			return occurrence, true
		}
	}
	return codeindex.Occurrence{}, false
}

// evidenceSelection is one candidate's chosen definition occurrence awaiting
// hydration. Selection reads only the projection; canonical byte access waits
// for hydration authorization.
type evidenceSelection struct {
	file       codeindex.FileProjection
	occurrence codeindex.Occurrence
}

// blockLines extracts the definition's statement block: its line plus every
// following non-blank line, bounded by maxBlockLines. The rule is deliberately
// lexical and deterministic; it makes no syntactic completeness claim.
func blockLines(content string, definitionLine uint32, maxBlockLines int) []string {
	lines := strings.Split(content, "\n")
	if definitionLine == 0 || int(definitionLine) > len(lines) {
		return nil
	}
	end := int(definitionLine) - 1
	block := make([]string, 0, maxBlockLines)
	for end < len(lines) && len(block) < maxBlockLines {
		if len(block) > 0 && strings.TrimSpace(lines[end]) == "" {
			break
		}
		block = append(block, lines[end])
		end++
	}
	return block
}
