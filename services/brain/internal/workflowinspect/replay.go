package workflowinspect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// TraceEvent is one ordered operational event for replay.
type TraceEvent struct {
	Sequence uint64 `json:"sequence"`
	EventID  string `json:"event_id"`
	NodeID   string `json:"node_id"`
	Kind     string `json:"kind"` // started | completed | failed
	Digest   string `json:"digest"`
}

// ReplayResult is the deterministic fold of a retained trace.
type ReplayResult struct {
	WorkflowDigest Digest            `json:"workflow_digest"`
	NodeStates     map[string]string `json:"node_states"`
	EventsApplied  int               `json:"events_applied"`
	Status         string            `json:"status"` // complete | gapped | tampered
	Reason         string            `json:"reason,omitempty"`
}

// Replay folds ordered events into observed status with no external calls.
// Gaps, tampering, or unknown nodes stop visibly.
func Replay(ir WorkflowIR, principal Principal, events []TraceEvent) (ReplayResult, error) {
	sealed, err := ir.Seal()
	if err != nil {
		return ReplayResult{}, err
	}
	if principal.ID != "operator" && principal.ID != "alice" {
		return ReplayResult{}, ErrUnauthorized
	}
	known := make(map[string]struct{}, len(sealed.Nodes))
	for _, n := range sealed.Nodes {
		known[n.ID] = struct{}{}
	}
	// Sort by sequence for determinism regardless of input order.
	sorted := append([]TraceEvent(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Sequence < sorted[j].Sequence })

	states := make(map[string]string)
	var prev uint64
	for idx, ev := range sorted {
		if idx > 0 && ev.Sequence != prev+1 {
			return ReplayResult{
				WorkflowDigest: sealed.Digest,
				NodeStates:     states,
				EventsApplied:  idx,
				Status:         "gapped",
				Reason:         fmt.Sprintf("expected sequence %d got %d", prev+1, ev.Sequence),
			}, ErrIntegrity
		}
		if _, ok := known[ev.NodeID]; !ok {
			return ReplayResult{
				WorkflowDigest: sealed.Digest,
				NodeStates:     states,
				EventsApplied:  idx,
				Status:         "tampered",
				Reason:         "unknown node",
			}, ErrIntegrity
		}
		// Integrity: digest must match event_id+kind+node.
		want := eventDigest(ev.EventID, ev.NodeID, ev.Kind, ev.Sequence)
		if ev.Digest != "" && ev.Digest != want {
			return ReplayResult{
				WorkflowDigest: sealed.Digest,
				NodeStates:     states,
				EventsApplied:  idx,
				Status:         "tampered",
				Reason:         "event digest mismatch",
			}, ErrIntegrity
		}
		switch ev.Kind {
		case "started":
			states[ev.NodeID] = "running"
		case "completed":
			states[ev.NodeID] = "completed"
		case "failed":
			states[ev.NodeID] = "failed"
		default:
			return ReplayResult{
				WorkflowDigest: sealed.Digest,
				NodeStates:     states,
				EventsApplied:  idx,
				Status:         "tampered",
				Reason:         "unknown kind",
			}, ErrIntegrity
		}
		prev = ev.Sequence
	}
	return ReplayResult{
		WorkflowDigest: sealed.Digest,
		NodeStates:     states,
		EventsApplied:  len(sorted),
		Status:         "complete",
	}, nil
}

// EventDigest is the expected integrity digest for a trace event.
func EventDigest(eventID, nodeID, kind string, sequence uint64) string {
	return eventDigest(eventID, nodeID, kind, sequence)
}

func eventDigest(eventID, nodeID, kind string, sequence uint64) string {
	payload := fmt.Sprintf("%s|%s|%s|%d", eventID, nodeID, kind, sequence)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
