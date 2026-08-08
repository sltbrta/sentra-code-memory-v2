package connector

import (
	"context"
	"time"
)

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
	ID          string
	Kind        ObjectKind
	Title       string
	Body        string
	Path        string
	IssueNumber int
	StartLine   int
	EndLine     int
	Version     string
	Deleted     bool
}

// SnapshotPage is one complete or partial provider page.
type SnapshotPage struct {
	Cursor          string
	Revision        string
	ConnectorDigest string
	ObservedAt      time.Time
	Objects         []Object
	Complete        bool
	ErrorCode       string
}

// SourceAPI is the narrow GitHub source surface used by the connector kernel.
// Implementations live in this package (fake) or are injected by the gateway
// with a live REST adapter that does not force brain→broker/internal imports.
type SourceAPI interface {
	Snapshot(ctx context.Context, owner, repo string) (SnapshotPage, error)
	Delta(ctx context.Context, owner, repo, priorCursor string) (SnapshotPage, error)
}
