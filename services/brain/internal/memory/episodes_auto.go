package memory

import (
	"fmt"
	"regexp"
	"time"
)

var (
	reMeeting  = regexp.MustCompile(`(?i)\b(meeting|standup|sync|retro|1:1|one-on-one)\b`)
	reIncident = regexp.MustCompile(`(?i)\b(incident|outage|sev[-\s]?[0-3]|postmortem|pager)\b`)
	reDeploy   = regexp.MustCompile(`(?i)\b(deploy|release|shipped|rollout|launch)\b`)
)

// AutoSegmentCompanyLife binds episodes from doc titles/bodies
// (GAP-MEM-EPISODE-LIFE MVP: meeting / incident / deploy kinds).
func (s *Store) AutoSegmentCompanyLife(docs map[string]string) int {
	if s == nil || len(docs) == 0 {
		return 0
	}
	now := time.Now().UTC()
	n := 0
	for id, text := range docs {
		kind, title := classifyLifeDoc(id, text)
		if kind == "" {
			continue
		}
		epID := fmt.Sprintf("life-%s-%s", kind, id)
		if _, err := s.BindEpisode(Episode{
			ID: epID, Kind: kind, Title: title,
			DocumentIDs: []string{id},
			// Generation left empty — not generation-scoped.
		}); err == nil {
			n++
		}
		_ = now
	}
	return n
}

func classifyLifeDoc(id, text string) (kind, title string) {
	blob := id + "\n" + text
	head := blob
	if len(head) > 400 {
		head = head[:400]
	}
	switch {
	case reMeeting.MatchString(head):
		return "meeting", firstLineSummary(text, 80)
	case reIncident.MatchString(head):
		return "incident", firstLineSummary(text, 80)
	case reDeploy.MatchString(head):
		return "deploy", firstLineSummary(text, 80)
	default:
		return "", ""
	}
}
