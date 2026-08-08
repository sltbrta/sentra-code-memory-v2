package handover

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

// ErrDenied is the single non-disclosing denial.
var ErrDenied = errors.New("handover: not_found_or_denied")

// ErrRejected reports a typed validation failure.
var ErrRejected = errors.New("handover: rejected")

// ErrStaleFence rejects commits under a non-current fence.
var ErrStaleFence = errors.New("handover: stale_fence")

// ErrDuplicate is returned when an effect/completion was already committed.
var ErrDuplicate = errors.New("handover: already_completed")

// ErrIncompatibleRealm rejects non-certified or disallowed targets.
var ErrIncompatibleRealm = errors.New("handover: incompatible_realm")

// ErrRevoked stops hydration/effects after revoke.
var ErrRevoked = errors.New("handover: revoked")

// RealmKind is a certified execution realm.
type RealmKind string

const (
	RealmLocal RealmKind = "local"
	RealmModal RealmKind = "modal"
)

// Checkpoint is the portable immutable task state (no secrets).
type Checkpoint struct {
	TaskID            string            `json:"task_id"`
	WorkflowVersion   string            `json:"workflow_version"`
	BundleDigest      string            `json:"bundle_digest"`
	MailboxCursor     uint64            `json:"mailbox_cursor"`
	CompletedSteps    []string          `json:"completed_steps"`
	CapabilityReqs    []string          `json:"capability_requirements"`
	CreatedUnderFence uint64            `json:"created_under_fence"`
	CheckpointDigest  string            `json:"checkpoint_digest"`
	CreatedAt         time.Time         `json:"created_at"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// SealCheckpoint fills the content digest.
// CapabilityReqs and Metadata are sealed so capability-affecting fields
// cannot change without changing the digest.
func SealCheckpoint(c Checkpoint) Checkpoint {
	payload := c.TaskID + "|" + c.WorkflowVersion + "|" + c.BundleDigest + "|" +
		hexUint(c.MailboxCursor) + "|" + hexUint(c.CreatedUnderFence)
	for _, s := range c.CompletedSteps {
		payload += "|" + s
	}
	for _, req := range c.CapabilityReqs {
		payload += "|cap:" + req
	}
	if len(c.Metadata) > 0 {
		keys := make([]string, 0, len(c.Metadata))
		for k := range c.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			payload += "|meta:" + k + "=" + c.Metadata[k]
		}
	}
	sum := sha256.Sum256([]byte(payload))
	c.CheckpointDigest = hex.EncodeToString(sum[:])
	return c
}

func hexUint(v uint64) string {
	return hex.EncodeToString([]byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	})
}

// HandoverState is the safe-point lifecycle.
type HandoverState string

const (
	StateRunningSource HandoverState = "running_source"
	StateQuiescing     HandoverState = "quiescing"
	StatePrepared      HandoverState = "prepared"
	StateFenceAdvanced HandoverState = "fence_advanced"
	StateReplaying     HandoverState = "replaying"
	StateRunningTarget HandoverState = "running_target"
	StateCompleted     HandoverState = "completed"
	StatePaused        HandoverState = "paused_recoverable"
	StateCancelled     HandoverState = "cancelled"
)

// CompletionReceipt is the exactly-once fenced completion.
type CompletionReceipt struct {
	TaskID       string    `json:"task_id"`
	Fence        uint64    `json:"fence"`
	Realm        RealmKind `json:"realm"`
	Attempt      int       `json:"attempt"`
	ResultDigest string    `json:"result_digest"`
	CommittedAt  time.Time `json:"committed_at"`
}

// CleanupReceipt records removed transient resources.
type CleanupReceipt struct {
	WorkspacesRemoved []string `json:"workspaces_removed"`
	ModalAppsClosed   []string `json:"modal_apps_closed"`
	BundlesPurged     []string `json:"bundles_purged"`
	Complete          bool     `json:"complete"`
}
