//go:build linux || darwin

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeNativeHistorySkipsSnapshotPrefixAndRemainsBrowseOnly(t *testing.T) {
	config := t.TempDir()
	cwd := t.TempDir()
	project := filepath.Join(config, "projects", "project-hash")
	if err := os.MkdirAll(filepath.Join(project, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(project, "session.jsonl")
	data := "" +
		`{"type":"file-history-snapshot","snapshot":{"opaque":true}}` + "\n" +
		`{"type":"user","uuid":"user-1","parentUuid":null,"sessionId":"claude-source","cwd":"` + cwd + `","timestamp":"2026-09-19T10:00:00Z","message":{"role":"user","content":"Investigate history import"}}` + "\n" +
		`{"type":"assistant","uuid":"assistant-1","parentUuid":"user-1","sessionId":"claude-source","cwd":"` + cwd + `","timestamp":"2026-09-19T10:00:01Z","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"I found it"}]}}` + "\n" +
		`{"type":"assistant","uuid":"sidechain","parentUuid":"user-1","isSidechain":true,"sessionId":"claude-source","cwd":"` + cwd + `","timestamp":"2026-09-19T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"do not import"}]}}` + "\n"
	if err := os.WriteFile(session, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "subagents", "child.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := New("claude", Config{Env: map[string]string{"CLAUDE_CONFIG_DIR": config}})
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := backend.(NativeSessionProvider)
	if !ok {
		t.Fatal("Claude must expose browse-only native history")
	}
	if _, ok := backend.(NativeSessionImporter); ok {
		t.Fatal("Claude must not advertise native import without a proven CLI fork")
	}
	page, err := provider.ListNativeSessions(context.Background(), NativeSessionListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 {
		t.Fatalf("listed sessions = %+v", page)
	}
	summary := page.Sessions[0]
	if summary.Preview != "Investigate history import" || summary.Title != summary.Preview || summary.Model != "claude-test" {
		t.Fatalf("bounded Claude metadata = %+v", summary)
	}
	snapshot, err := provider.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: summary.Handle, Revision: summary.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ResumeSessionID != "" || len(snapshot.Messages) != 2 || snapshot.Messages[1].Content != "I found it" {
		t.Fatalf("Claude source snapshot = %+v", snapshot)
	}
}
