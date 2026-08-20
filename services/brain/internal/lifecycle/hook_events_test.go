package lifecycle

import (
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/testsupport"
)

// RunHook returned nil without doing anything, behind ~1,400 lines of installer
// that writes executables into a user's repository and rewrites their
// core.hooksPath. Separately sessionlog.Append had no non-test caller, so
// session_continuation and session_recall always answered from an empty log.
// Git lifecycle events are exactly the durable local facts that log exists to
// hold, so one wiring closes both gaps.

func sessionEvents(t *testing.T, root string) []sessionlog.Event {
	t.Helper()
	writer, err := sessionlog.Open(filepath.Join(root, ".sentra", "sessions"))
	if err != nil {
		t.Fatalf("open session log: %v", err)
	}
	return writer.Events()
}

func TestRunHookRecordsAnEventInTheSessionLog(t *testing.T) {
	root := testsupport.GitRepo(t, map[string]string{"a.go": "package a\n"})

	if err := RunHook(string(HookPostCommit), root); err != nil {
		t.Fatalf("RunHook: %v", err)
	}

	events := sessionEvents(t, root)
	if len(events) != 1 {
		t.Fatalf("session log holds %d events, want 1: the hook recorded nothing", len(events))
	}
	if events[0].Verb != "hooks_local:post-commit" {
		t.Fatalf("event verb = %q, want hooks_local:post-commit", events[0].Verb)
	}
	if events[0].Provenance.Tree == "" {
		t.Fatal("event carries no tree provenance; HEAD was not resolved")
	}
}

func TestRunHookClassifiesEventsByHookKind(t *testing.T) {
	for _, test := range []struct {
		hook HookKind
		want sessionlog.Kind
	}{
		{HookPostCommit, sessionlog.KindCompletion},
		{HookPrePush, sessionlog.KindCompletion},
		{HookPostCheckout, sessionlog.KindRefresh},
		{HookPostMerge, sessionlog.KindRefresh},
	} {
		t.Run(string(test.hook), func(t *testing.T) {
			root := testsupport.GitRepo(t, map[string]string{"a.go": "package a\n"})
			if err := RunHook(string(test.hook), root); err != nil {
				t.Fatalf("RunHook: %v", err)
			}
			events := sessionEvents(t, root)
			if len(events) != 1 {
				t.Fatalf("want 1 event, got %d", len(events))
			}
			if events[0].Kind != test.want {
				t.Fatalf("%s recorded kind %q, want %q", test.hook, events[0].Kind, test.want)
			}
		})
	}
}

// TestRunHookNeverFailsTheGitCommand is the constraint that governs everything
// here: a hook exiting non-zero blocks the user's commit or push.
func TestRunHookNeverFailsTheGitCommand(t *testing.T) {
	cases := map[string]string{
		"unknown hook kind":  "not-a-hook",
		"empty event":        "",
		"traversal as event": "../../etc/passwd",
	}
	root := testsupport.GitRepo(t, map[string]string{"a.go": "package a\n"})
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			if err := RunHook(event, root); err != nil {
				t.Fatalf("RunHook(%q) returned %v; a hook must never fail the git command", event, err)
			}
		})
	}
	// A root that is not a repository, and one that does not exist at all.
	if err := RunHook(string(HookPostCommit), t.TempDir()); err != nil {
		t.Fatalf("non-repository root returned %v", err)
	}
	if err := RunHook(string(HookPostCommit), filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("absent root returned %v", err)
	}
}
