package hosted

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Product chat channel (ported from Python product_brain/sessions + turns).
// Turns are conversational context, not company-document evidence IDs.

// isConversationPassage reports whether a passage is chat/turn/agent-memory lane
// (never a company cite). Agent memory uses DocumentID prefix "agent:" and
// Channel "agent_memory" — context only, never sole citation authority.
func isConversationPassage(p Passage) bool {
	if strings.HasPrefix(p.DocumentID, "turn:") || strings.HasPrefix(p.DocumentID, "agent:") {
		return true
	}
	ch := strings.ToLower(p.Channel)
	return strings.Contains(ch, "turn_grep") ||
		strings.Contains(ch, "conversation") ||
		strings.Contains(ch, "agent_memory")
}

// filterCompanyPassages drops conversation-lane passages from citation/evidence pools.
func filterCompanyPassages(passages []Passage) []Passage {
	out := make([]Passage, 0, len(passages))
	for _, p := range passages {
		if isConversationPassage(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

var sessionMu sync.Mutex
var turnTokRE = regexp.MustCompile(`[A-Za-z0-9_]{3,}`)

// SessionTurn is one JSONL row in the product session store.
type SessionTurn struct {
	SessionID string         `json:"session_id"`
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	TS        float64        `json:"ts"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// defaultSessionPath resolves OUROBOROS_BRAIN_SESSION_PATH or STATE_DIR default.
func defaultSessionPath() string {
	if p := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_SESSION_PATH")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_TURNS_PATH")); p != "" {
		return p
	}
	dir := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_STATE_DIR"))
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "ouroboros-brain")
	}
	return filepath.Join(dir, "sessions.jsonl")
}

// AppendSessionTurn appends one turn to the product session JSONL store.
func AppendSessionTurn(sessionID, role, content string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	path := defaultSessionPath()
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(content) > 16000 {
		content = content[:16000]
	}
	row := SessionTurn{
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		TS:        float64(time.Now().UnixNano()) / 1e9,
		Meta:      map[string]any{},
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// LoadSessionTurns returns the last limit turns for a session (chronological).
func LoadSessionTurns(sessionID string, limit int) []SessionTurn {
	if limit <= 0 {
		limit = 40
	}
	path := defaultSessionPath()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []SessionTurn
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row SessionTurn
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if sessionID != "" && row.SessionID != sessionID {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	return rows
}

// FormatSessionHistory renders labelled history for the answer prompt (not evidence).
func FormatSessionHistory(sessionID string, limit int) string {
	turns := LoadSessionTurns(sessionID, limit)
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Conversation history (not source evidence):\n")
	n := 0
	for _, t := range turns {
		content := strings.TrimSpace(strings.ReplaceAll(t.Content, "\n", " "))
		if content == "" {
			continue
		}
		if len(content) > 800 {
			content = content[:800]
		}
		role := t.Role
		if role == "" {
			role = "user"
		}
		b.WriteString("- [")
		b.WriteString(role)
		b.WriteString("] ")
		b.WriteString(content)
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		return ""
	}
	return b.String()
}

func turnTokens(text string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, t := range turnTokRE.FindAllString(strings.ToLower(text), -1) {
		m[t] = struct{}{}
	}
	return m
}

// TurnHit is a turn_grep result (conversation lane — not company evidence).
type TurnHit struct {
	DocumentID string
	Text       string
	Score      float64
	Role       string
}

// TurnGrep searches session turns by token overlap (ported from product_brain/turns).
// sessionID empty = global search across all sessions.
func TurnGrep(question, sessionID string, topK, minOverlap int) ([]TurnHit, map[string]any) {
	if topK <= 0 {
		topK = 8
	}
	if minOverlap <= 0 {
		minOverlap = 2
	}
	path := defaultSessionPath()
	diag := map[string]any{"channel": "turn_grep", "path": path}
	f, err := os.Open(path)
	if err != nil {
		diag["status"] = "no_turns_file"
		return nil, diag
	}
	defer f.Close()
	qToks := turnTokens(question)
	if len(qToks) == 0 {
		diag["status"] = "empty_query"
		return nil, diag
	}
	type scored struct {
		score float64
		hit   TurnHit
	}
	var scoredHits []scored
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row SessionTurn
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if sessionID != "" && row.SessionID != sessionID {
			continue
		}
		body := row.Content
		btoks := turnTokens(body)
		overlap := 0
		for t := range btoks {
			if _, ok := qToks[t]; ok {
				overlap++
			}
		}
		if overlap < minOverlap {
			continue
		}
		score := float64(overlap) / float64(len(qToks))
		// Nanosecond-resolution id so same-second turns do not collide.
		id := fmt.Sprintf("turn:%s:%.9f", row.SessionID, row.TS)
		text := body
		if len(text) > 4000 {
			text = text[:4000]
		}
		scoredHits = append(scoredHits, scored{
			score: score,
			hit: TurnHit{
				DocumentID: id,
				Text:       text,
				Score:      score,
				Role:       row.Role,
			},
		})
	}
	sort.Slice(scoredHits, func(i, j int) bool {
		return scoredHits[i].score > scoredHits[j].score
	})
	if len(scoredHits) > topK {
		scoredHits = scoredHits[:topK]
	}
	out := make([]TurnHit, len(scoredHits))
	for i, s := range scoredHits {
		out[i] = s.hit
	}
	diag["status"] = "ok"
	diag["hits"] = len(out)
	return out, diag
}

// LongContextFallback builds a labelled long-context window from recent turns.
func LongContextFallback(sessionID string, maxChars int) (string, map[string]any) {
	if maxChars <= 0 {
		maxChars = 6000
	}
	turns := LoadSessionTurns(sessionID, 40)
	diag := map[string]any{"channel": "long_context_fallback", "turns": len(turns)}
	if len(turns) == 0 {
		diag["status"] = "empty"
		return "", diag
	}
	var b strings.Builder
	b.WriteString("Recent conversation context (not company evidence):\n")
	used := 0
	// Chronological order; stop when budget exhausted (keep newest by scanning from end into prefix budget).
	start := 0
	total := 0
	for i := len(turns) - 1; i >= 0; i-- {
		line := "[" + turns[i].Role + "] " + strings.TrimSpace(turns[i].Content) + "\n"
		if total+len(line) > maxChars {
			start = i + 1
			break
		}
		total += len(line)
		start = i
	}
	for i := start; i < len(turns); i++ {
		line := "[" + turns[i].Role + "] " + strings.TrimSpace(turns[i].Content) + "\n"
		b.WriteString(line)
		used += len(line)
	}
	diag["status"] = "ok"
	diag["chars"] = used
	return b.String(), diag
}
