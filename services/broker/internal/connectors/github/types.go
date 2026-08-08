package github

import (
	"context"
	"os"
	"strings"
)

// TokenEnvNames are the fine-grained PAT env vars resolved for live mode.
var TokenEnvNames = []string{"GITHUB_TOKEN", "OUROBOROS_GITHUB_TOKEN"}

// ObjectKind identifies one admitted provider object family.
type ObjectKind string

const (
	// ObjectKindRepository is repository metadata evidence.
	ObjectKindRepository ObjectKind = "repository"
	// ObjectKindIssue is issue evidence.
	ObjectKindIssue ObjectKind = "issue"
	// ObjectKindFile is repository-file evidence.
	ObjectKindFile ObjectKind = "file"
)

// Object is one immutable provider evidence projection.
type Object struct {
	// ID is the stable provider object identity (e.g. issue:42, file:README.md).
	ID string
	// Kind classifies the object.
	Kind ObjectKind
	// Title is a short display title.
	Title string
	// Body is the searchable prose.
	Body string
	// Path is set for file objects.
	Path string
	// IssueNumber is set for issue objects.
	IssueNumber int
	// StartLine / EndLine bound file spans (1-based inclusive start, exclusive end).
	StartLine int
	EndLine   int
	// Version is the opaque provider revision token.
	Version string
	// Deleted marks a confirmed provider deletion (complete reconcile only).
	Deleted bool
}

// SnapshotPage is one complete or partial provider page.
type SnapshotPage struct {
	// Cursor is the opaque resume cursor after this page when Complete.
	Cursor string
	// Objects are the page contents.
	Objects []Object
	// Complete is false for partial, rate-limited, or malformed pages.
	Complete bool
	// ErrorCode is a stable non-sensitive provider error when Complete is false.
	ErrorCode string
}

// SourceAPI is the narrow GitHub source surface used by the connector kernel.
type SourceAPI interface {
	// Snapshot returns one complete baseline page for owner/repo.
	Snapshot(ctx context.Context, owner, repo string) (SnapshotPage, error)
	// Delta returns objects newer than priorCursor. Complete false never implies deletion.
	Delta(ctx context.Context, owner, repo, priorCursor string) (SnapshotPage, error)
}

// ResolveToken reads GITHUB_TOKEN or OUROBOROS_GITHUB_TOKEN.
func ResolveToken() string {
	for _, name := range TokenEnvNames {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
