package hosted

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var tokenRE = regexp.MustCompile(`[A-Za-z0-9_]{2,}`)

const lexicalORSQL = `
SELECT chunk_id, dsid, text_content, COALESCE(source_uri, '') AS source_uri,
       ts_rank(tsv, to_tsquery('english', $2)) AS rank
FROM path2_chunk_metadata
WHERE brain_id = $1 AND tsv @@ to_tsquery('english', $2)
ORDER BY rank DESC
LIMIT $3
`

const lexicalWebSQL = `
SELECT chunk_id, dsid, text_content, COALESCE(source_uri, '') AS source_uri,
       ts_rank(tsv, websearch_to_tsquery('english', $2)) AS rank
FROM path2_chunk_metadata
WHERE brain_id = $1 AND tsv @@ websearch_to_tsquery('english', $2)
ORDER BY rank DESC
LIMIT $3
`

const siblingsSQL = `
SELECT chunk_id, text_content, COALESCE(source_uri, '') AS source_uri
FROM path2_chunk_metadata
WHERE brain_id = $1 AND dsid = $2
ORDER BY chunk_id
LIMIT $3
`

// siblingsPreferDatesSQL pulls date-bearing chunks first so atomic "when/what
// date" answers do not miss mid-document timeline lines when LIMIT truncates.
const siblingsPreferDatesSQL = `
SELECT chunk_id, text_content, COALESCE(source_uri, '') AS source_uri
FROM path2_chunk_metadata
WHERE brain_id = $1 AND dsid = $2
ORDER BY
  CASE WHEN text_content ~ '[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN 0 ELSE 1 END,
  chunk_id
LIMIT $3
`

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// Prod path: few concurrent FTS/hydrate queries — keep pool modest to
	// avoid Neon connection storms under Modal burst.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(3 * time.Minute)
	db.SetConnMaxIdleTime(90 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func orTSQuery(question string, maxTerms int) string {
	if maxTerms <= 0 {
		maxTerms = 24
	}
	seen := map[string]struct{}{}
	var terms []string
	for _, tok := range tokenRE.FindAllString(strings.ToLower(question), -1) {
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		safe := strings.ReplaceAll(tok, "'", "''")
		terms = append(terms, safe)
		if len(terms) >= maxTerms {
			break
		}
	}
	return strings.Join(terms, " | ")
}

// lexicalSearch runs one Neon FTS query. maxTerms caps OR-tsquery width (prod: 12).
// Prefer a short per-call timeout via ctx so multi-arm fan-out cannot hang forever.
func lexicalSearch(ctx context.Context, db *sql.DB, cfg Config, question string) ([]Hit, error) {
	return lexicalSearchLimited(ctx, db, cfg, question, 24, 0)
}

func lexicalSearchLimited(ctx context.Context, db *sql.DB, cfg Config, question string, maxTerms, limit int) ([]Hit, error) {
	if maxTerms <= 0 {
		maxTerms = 24
	}
	if limit <= 0 {
		limit = cfg.LexicalLimit
	}
	if limit <= 0 {
		limit = 20
	}
	fts := orTSQuery(question, maxTerms)
	var rows *sql.Rows
	var err error
	if fts != "" {
		rows, err = db.QueryContext(ctx, lexicalORSQL, cfg.BrainID, fts, limit)
	}
	if err != nil || rows == nil {
		if rows != nil {
			_ = rows.Close()
		}
		// Fallback: websearch with truncated question (avoid multi-KB prompts).
		q := question
		q = textbound.Bytes(q, 400)
		rows, err = db.QueryContext(ctx, lexicalWebSQL, cfg.BrainID, q, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("neon lexical: %w", err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var rank float64
		if err := rows.Scan(&h.ChunkID, &h.DSID, &h.Text, &h.SourceURI, &rank); err != nil {
			return nil, err
		}
		h.Score = rank
		h.Channel = "lexical"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ProjectHotLexFromDSN opens Neon and streams path2 into HotLex (legacy serial).
// Prefer ProjectHotLexFromDSNFast for multi-worker keyset projection.
func ProjectHotLexFromDSN(ctx context.Context, dsn, brainID string, maxDocs, textChars int, stripText bool) (*HotLex, error) {
	res, err := ProjectHotLexFromDSNFast(ctx, dsn, ProjectOptions{
		BrainID:   brainID,
		MaxDocs:   maxDocs,
		TextChars: textChars,
		StripText: stripText,
		Workers:   8,
		Shards:    8,
		ShardID:   -1,
		PageSize:  5000,
	})
	if err != nil {
		return nil, err
	}
	return res.Index, nil
}

// ProjectHotLexFromNeon streams path2_chunk_metadata into a HotLex index.
// textChars caps body used for tokenization (and optional stored text).
// When stripText is true, only postings + meta are retained (hydrate-by-id at query).
// maxDocs <= 0 means all rows for brainID.
func ProjectHotLexFromNeon(ctx context.Context, db *sql.DB, brainID string, maxDocs, textChars int, stripText bool) (*HotLex, error) {
	if db == nil {
		return nil, fmt.Errorf("project hotlex: nil db")
	}
	if strings.TrimSpace(brainID) == "" {
		return nil, fmt.Errorf("project hotlex: empty brain_id")
	}
	if textChars <= 0 {
		textChars = 800
	}
	h := NewHotLex(brainID)
	// Server-side cursor via simple QueryContext (driver streams rows).
	q := `
SELECT chunk_id, dsid,
       left(text_content, $2) AS text_content,
       COALESCE(source_uri, '') AS source_uri
FROM path2_chunk_metadata
WHERE brain_id = $1
`
	rows, err := db.QueryContext(ctx, q, brainID, textChars)
	if err != nil {
		return nil, fmt.Errorf("project hotlex query: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("project hotlex: after %d rows: %w", n, err)
		}
		var chunkID, dsid, text, uri string
		if err := rows.Scan(&chunkID, &dsid, &text, &uri); err != nil {
			return nil, err
		}
		if stripText {
			h.AddChunkIndexOnly(chunkID, dsid, text, uri)
		} else {
			h.AddChunk(chunkID, dsid, text, uri)
		}
		n++
		if maxDocs > 0 && n >= maxDocs {
			break
		}
		// Progress to stderr so long Modal runs are observable.
		if n%100000 == 0 {
			fmt.Fprintf(os.Stderr, "project hotlex: indexed %d chunks\n", n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project hotlex rows: after %d: %w", n, err)
	}
	if stripText {
		h.StripStoredText() // belt-and-suspenders
	}
	if n == 0 {
		return nil, fmt.Errorf("project hotlex: zero rows for brain_id=%s", brainID)
	}
	fmt.Fprintf(os.Stderr, "project hotlex: done rows=%d strip_text=%v\n", n, stripText)
	return h, nil
}

// hydrateByChunkIDs loads full text for known chunk IDs (interactive path:
// search returns IDs from hotlex/ANN; Neon is source-of-truth hydrate only).
func hydrateByChunkIDs(ctx context.Context, db *sql.DB, cfg Config, chunkIDs []string) ([]Hit, error) {
	if db == nil || len(chunkIDs) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	// Build IN ($2,$3,...) — portable across drivers for small ID lists.
	args := make([]any, 0, 1+len(ids))
	args = append(args, cfg.BrainID)
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	q := fmt.Sprintf(`
SELECT chunk_id, dsid, text_content, COALESCE(source_uri, '') AS source_uri
FROM path2_chunk_metadata
WHERE brain_id = $1 AND chunk_id IN (%s)
`, strings.Join(placeholders, ","))
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("neon hydrate-by-id: %w", err)
	}
	defer rows.Close()
	byID := map[string]Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.DSID, &h.Text, &h.SourceURI); err != nil {
			return nil, err
		}
		h.Channel = "hydrate_id"
		byID[h.ChunkID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(ids))
	for _, id := range ids {
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// siblingChunks loads more chunks for top documents (whole-doc hydrate).
func siblingChunks(ctx context.Context, db *sql.DB, cfg Config, dsid string, limit int) ([]Hit, error) {
	return siblingChunksQuery(ctx, db, cfg, dsid, limit, siblingsSQL)
}

// siblingChunksPreferDates prefers ISO-date-bearing chunks when hydrating for
// date-seeking questions (generalized; no doc-id hardcoding).
func siblingChunksPreferDates(ctx context.Context, db *sql.DB, cfg Config, dsid string, limit int) ([]Hit, error) {
	return siblingChunksQuery(ctx, db, cfg, dsid, limit, siblingsPreferDatesSQL)
}

func siblingChunksQuery(ctx context.Context, db *sql.DB, cfg Config, dsid string, limit int, q string) ([]Hit, error) {
	if limit <= 0 {
		limit = 4
	}
	rows, err := db.QueryContext(ctx, q, cfg.BrainID, dsid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.Text, &h.SourceURI); err != nil {
			return nil, err
		}
		h.DSID = dsid
		h.Channel = "hydrate"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
