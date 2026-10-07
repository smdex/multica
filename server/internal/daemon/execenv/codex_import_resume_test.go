package execenv

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareCodexImportedResumeExposesSharedFork(t *testing.T) {
	sharedHome := t.TempDir()
	// Prepare must honor the import-time provider home even when the daemon's
	// ambient CODEX_HOME changed before this fresh post-restart execution.
	t.Setenv("CODEX_HOME", t.TempDir())
	const (
		resumeID = "018f0d34-9c36-7ace-86c2-8a6b13b5f001"
		chatID   = "018f0d34-9c36-7ace-86c2-8a6b13b5f002"
	)
	rolloutDir := filepath.Join(sharedHome, "sessions", "2026", "09", "19")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-2026-09-19T00-00-00-"+resumeID+".jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"thread\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := Prepare(PrepareParams{
		WorkspacesRoot:  t.TempDir(),
		WorkspaceID:     "workspace",
		TaskID:          "018f0d34-9c36-7ace-86c2-8a6b13b5f003",
		Provider:        "codex",
		ResumeSessionID: resumeID,
		CodexSourceHome: sharedHome,
		Task:            TaskContextForEnv{ChatSessionID: chatID},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer env.Cleanup(true)

	if !CodexResumeRolloutPresent(env.CodexHome, resumeID) {
		t.Fatalf("fresh task CODEX_HOME %q does not expose imported fork %q", env.CodexHome, resumeID)
	}
	got, err := os.ReadFile(filepath.Join(env.CodexHome, "sessions", "2026", "09", "19", filepath.Base(rollout)))
	if err != nil {
		t.Fatalf("read exposed imported fork: %v", err)
	}
	if string(got) != "{\"type\":\"thread\"}\n" {
		t.Fatalf("exposed imported fork = %q", got)
	}
}

func TestCanonicalSharedCodexHomeMakesUserHomeAbsolute(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "relative-home")
	if err := os.MkdirAll(filepath.Join(root, "relative-home", ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalSharedCodexHome()
	if err != nil {
		t.Fatalf("CanonicalSharedCodexHome: %v", err)
	}
	want := filepath.Join(root, "relative-home", ".codex")
	if got != want {
		t.Fatalf("canonical shared home = %q, want %q", got, want)
	}
}
