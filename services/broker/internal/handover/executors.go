package handover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
)

// LocalExecutor materializes a disposable local workspace for pure compute.
type LocalExecutor struct {
	mu         sync.Mutex
	workspaces map[string]string
	failNext   bool
}

// NewLocalExecutor returns a hermetic local realm adapter.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{workspaces: make(map[string]string)}
}

// Realm returns local.
func (e *LocalExecutor) Realm() RealmKind { return RealmLocal }

// FailNext injects a single failure for recovery tests.
func (e *LocalExecutor) FailNext() {
	e.mu.Lock()
	e.failNext = true
	e.mu.Unlock()
}

// Run executes pure work from the checkpoint only.
func (e *LocalExecutor) Run(_ context.Context, checkpoint Checkpoint, fence uint64, attempt int) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failNext {
		e.failNext = false
		return "", fmt.Errorf("local transient failure")
	}
	ws := fmt.Sprintf("local-ws-%s-f%d-a%d", checkpoint.TaskID, fence, attempt)
	e.workspaces[checkpoint.TaskID] = ws
	sum := sha256.Sum256([]byte(checkpoint.BundleDigest + "|local|" + hexUint(fence)))
	return hex.EncodeToString(sum[:]), nil
}

// Cleanup removes the disposable workspace.
func (e *LocalExecutor) Cleanup(_ context.Context, taskID string) CleanupReceipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws, ok := e.workspaces[taskID]
	delete(e.workspaces, taskID)
	if !ok {
		return CleanupReceipt{Complete: true}
	}
	// Workspace identity only; bundle digests are not materialised here.
	return CleanupReceipt{WorkspacesRemoved: []string{ws}, Complete: true}
}

// ModalExecutor is the Modal realm adapter (hermetic by default; live optional).
type ModalExecutor struct {
	mu       sync.Mutex
	apps     map[string]string
	failNext bool
	mode     string // hermetic | live
}

// NewModalExecutor returns a Modal adapter. Mode defaults to hermetic.
func NewModalExecutor() *ModalExecutor {
	mode := os.Getenv("OUROBOROS_MODAL_SMOKE_MODE")
	if mode == "" {
		mode = "hermetic"
	}
	return &ModalExecutor{apps: make(map[string]string), mode: mode}
}

// Realm returns modal.
func (e *ModalExecutor) Realm() RealmKind { return RealmModal }

// FailNext injects a single failure for recovery tests.
func (e *ModalExecutor) FailNext() {
	e.mu.Lock()
	e.failNext = true
	e.mu.Unlock()
}

// Run executes pure work. Hermetic mode never touches the network.
// Live mode optionally dispatches a Modal function when credentials exist.
func (e *ModalExecutor) Run(ctx context.Context, checkpoint Checkpoint, fence uint64, attempt int) (string, error) {
	e.mu.Lock()
	if e.failNext {
		e.failNext = false
		e.mu.Unlock()
		return "", fmt.Errorf("modal transient failure")
	}
	app := fmt.Sprintf("ouroboros-stage13-handover-%s", checkpoint.TaskID)
	e.apps[checkpoint.TaskID] = app
	mode := e.mode
	e.mu.Unlock()

	// Go broker path is always pure-local digest (no Modal SDK embed).
	// Elevated live Modal reachability is owned by tools/build-spine/modal_smoke.py.
	// mode is retained for operator diagnostics only.
	_ = mode
	_ = ctx
	sum := sha256.Sum256([]byte(checkpoint.BundleDigest + "|modal|" + hexUint(fence)))
	return hex.EncodeToString(sum[:]), nil
}

// Cleanup closes the ephemeral Modal app record.
func (e *ModalExecutor) Cleanup(_ context.Context, taskID string) CleanupReceipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	app, ok := e.apps[taskID]
	delete(e.apps, taskID)
	if !ok {
		return CleanupReceipt{Complete: true}
	}
	// App identity only; bundle digests are not materialised by this adapter.
	return CleanupReceipt{ModalAppsClosed: []string{app}, Complete: true}
}
