//go:build unix

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCodexNativeHistoryListsReadsAndForksOnlyOwnedThread(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	logPath := filepath.Join(root, "requests.jsonl")
	fake := filepath.Join(root, "codex")
	thread := func(id string, updated int) string {
		return fmt.Sprintf("{\"thread\":{\"id\":%q,\"cwd\":%q,\"updatedAt\":%d,\"name\":\"Native task\",\"preview\":\"inspect native history\",\"turns\":[{\"items\":[{\"type\":\"userMessage\",\"id\":\"u1\",\"content\":\"inspect native history\"},{\"type\":\"agentMessage\",\"id\":\"a1\",\"text\":\"history found\"},{\"type\":\"reasoning\",\"id\":\"r1\",\"summary\":[\"checking\"]}]}]}}", id, cwd, updated)
	}
	script := fmt.Sprintf("#!/bin/sh\n"+
		"id_for() { printf '%%s' \"$1\" | sed -n 's/.*\"id\":\\([0-9][0-9]*\\).*/\\1/p'; }\n"+
		"while IFS= read -r line; do\n"+
		"  printf '%%s\\n' \"$line\" >> %q\n"+
		"  id=$(id_for \"$line\")\n"+
		"  case \"$line\" in\n"+
		"    *'\"method\":\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":{}}\\n' \"$id\" ;;\n"+
		"    *'\"method\":\"thread/list\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":{\"data\":[{\"id\":\"source-thread\",\"cwd\":%q,\"updatedAt\":1,\"name\":\"Native task\",\"preview\":\"inspect native history\"}],\"nextCursor\":\"\"}}\\n' \"$id\" ;;\n"+
		"    *'\"method\":\"thread/fork\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":{\"thread\":{\"id\":\"owned-thread\"}}}\\n' \"$id\" ;;\n"+
		"    *'\"method\":\"thread/read\"'*'\"threadId\":\"source-thread\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":%%s}\\n' \"$id\" %q ;;\n"+
		"    *'\"method\":\"thread/read\"'*'\"threadId\":\"owned-thread\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":%%s}\\n' \"$id\" %q ;;\n"+
		"    *'\"method\":\"initialized\"'*) : ;;\n"+
		"    *) exit 29 ;;\n"+
		"  esac\n"+
		"done\n", logPath, cwd, thread("source-thread", 1), thread("owned-thread", 2))
	writeTestExecutable(t, fake, []byte(script))
	backend, err := New("codex", Config{ExecutablePath: fake})
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := backend.(NativeSessionProvider)
	if !ok {
		t.Fatal("Codex must expose native history")
	}
	importer, ok := backend.(NativeSessionImporter)
	if !ok {
		t.Fatal("Codex must expose native import")
	}
	page, err := provider.ListNativeSessions(context.Background(), NativeSessionListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].Handle != "source-thread" {
		t.Fatalf("Codex list = %+v", page)
	}
	listedLog, _ := os.ReadFile(logPath)
	if strings.Contains(string(listedLog), "\"method\":\"thread/read\"") {
		t.Fatalf("Codex list hydrated a thread: %s", listedLog)
	}
	source := page.Sessions[0]
	browsed, err := provider.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: source.Handle, Revision: source.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if browsed.ResumeSessionID != "" || len(browsed.Messages) != 2 || browsed.Messages[1].Content != "history found" || browsed.Messages[0].CreatedAt.IsZero() || browsed.Messages[1].CreatedAt.IsZero() || !strings.Contains(strings.Join(browsed.Warnings, "\n"), codexNativeMissingTimestampWarning) {
		t.Fatalf("Codex browsed source = %+v", browsed)
	}
	opts := NativeSessionPrepareOptions{ImportID: uuid.NewString(), Handle: source.Handle, Revision: source.Revision, DestinationDir: filepath.Join(root, "preparation")}
	prepared, err := importer.PrepareNativeSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ResumeSessionID != "owned-thread" || prepared.Snapshot.ResumeSessionID != "owned-thread" {
		t.Fatalf("Codex prepared ownership = %+v", prepared)
	}
	_, stagePath, err := nativePreparationPaths(opts, "codex")
	if err != nil {
		t.Fatal(err)
	}
	intent, exists, err := readNativePreparationIntent(nativePreparationIntentPath(stagePath))
	if err != nil || !exists || intent.Phase != "owned" || intent.OwnedHandle != "owned-thread" || intent.OwnedRevision != prepared.Snapshot.Summary.Revision {
		t.Fatalf("validated owned intent = %+v, %v", intent, err)
	}
	if _, err := importer.PrepareNativeSession(context.Background(), opts); err != nil {
		t.Fatalf("Codex idempotent preparation: %v", err)
	}
	requests, _ := os.ReadFile(logPath)
	if strings.Count(string(requests), "\"method\":\"thread/fork\"") != 1 || strings.Contains(string(requests), "\"method\":\"thread/start\"") || strings.Contains(string(requests), "\"method\":\"turn/start\"") {
		t.Fatalf("unexpected Codex preparation traffic: %s", requests)
	}
	if !strings.Contains(string(requests), "\"ephemeral\":false") || !strings.Contains(string(requests), "\"excludeTurns\":false") {
		t.Fatalf("Codex fork did not use exact ownership parameters: %s", requests)
	}
}

func TestCodexNativeHistoryRejectsOversizedProtocolResponse(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "codex")
	script := fmt.Sprintf("#!/bin/sh\n"+
		"id_for() { printf '%%s' \"$1\" | sed -n 's/.*\"id\":\\([0-9][0-9]*\\).*/\\1/p'; }\n"+
		"while IFS= read -r line; do\n"+
		"  id=$(id_for \"$line\")\n"+
		"  case \"$line\" in\n"+
		"    *'\"method\":\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":{}}\\n' \"$id\" ;;\n"+
		"    *'\"method\":\"initialized\"'*) : ;;\n"+
		"    *'\"method\":\"thread/read\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%%s,\"result\":{\"filler\":\"' \"$id\"; head -c %d /dev/zero; printf '\"}}\\n' ;;\n"+
		"    *) exit 29 ;;\n"+
		"  esac\n"+
		"done\n", codexNativeMaxResponseBytes+1)
	writeTestExecutable(t, fake, []byte(script))
	backend, err := New("codex", Config{ExecutablePath: fake})
	if err != nil {
		t.Fatal(err)
	}
	provider := backend.(NativeSessionProvider)
	_, err = provider.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: "source-thread"})
	if nativeSessionErrorCode(err) != NativeSessionHistoryTooLarge {
		t.Fatalf("oversized Codex protocol response = %v (%s)", err, nativeSessionErrorCode(err))
	}
}

func TestCodexNativeSnapshotRejectsInvalidTurn(t *testing.T) {
	_, err := codexNativeSnapshot(map[string]any{
		"id":        "thread",
		"cwd":       t.TempDir(),
		"updatedAt": 1,
		"turns":     []any{"invalid-turn"},
	})
	if nativeSessionErrorCode(err) != NativeSessionInvalidHistory {
		t.Fatalf("invalid Codex turn error = %v (%s)", err, nativeSessionErrorCode(err))
	}
}

func TestCodexNativeSnapshotMapsSettledToolTimelineBeforeAssistantText(t *testing.T) {
	thread := map[string]any{
		"id":        "thread",
		"cwd":       t.TempDir(),
		"updatedAt": "2026-09-19T10:00:04Z",
		"turns": []any{map[string]any{
			"createdAt": "2026-09-19T10:00:00Z",
			"items": []any{
				map[string]any{"type": "userMessage", "id": "user", "content": "inspect the worktree"},
				map[string]any{"type": "reasoning", "id": "reasoning", "summary": []any{"checking state"}},
				map[string]any{"type": "commandExecution", "id": "command", "status": "completed", "command": "git status", "aggregatedOutput": "clean"},
				map[string]any{"type": "mcpToolCall", "id": "search", "status": "failed", "server": "plugin-search", "tool": "search", "arguments": map[string]any{"query": "native history"}, "error": map[string]any{"message": "Bearer secret-token-value"}},
				map[string]any{"type": "agentMessage", "id": "assistant", "text": "The worktree is clean."},
			},
		}},
	}

	snapshot, err := codexNativeSnapshotAt(thread, time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 2 || snapshot.Messages[1].Content != "The worktree is clean." {
		t.Fatalf("messages = %+v", snapshot.Messages)
	}
	events := snapshot.Messages[1].Events
	if len(events) != 5 {
		t.Fatalf("events = %+v", events)
	}
	wantTypes := []MessageType{MessageThinking, MessageToolUse, MessageToolResult, MessageToolUse, MessageToolResult}
	for index, want := range wantTypes {
		if events[index].Type != want {
			t.Fatalf("event %d type = %q, want %q: %+v", index, events[index].Type, want, events)
		}
	}
	if events[0].Content != "checking state" || events[1].Tool != "exec_command" || events[1].Input["command"] != "git status" || events[2].Output != "clean" {
		t.Fatalf("completed command events = %+v", events[:3])
	}
	if events[3].Tool != "search" || events[4].Tool != "search" || events[4].Status != "failed" || events[4].Output != "failed\nerror: Bearer [REDACTED]" {
		t.Fatalf("failed MCP events = %+v", events[3:])
	}
}

func TestCodexNativeSnapshotAssignsImportTimestampsForMissingNativeTimes(t *testing.T) {
	importedAt := time.Date(2026, time.September, 20, 12, 0, 0, 123, time.FixedZone("non-UTC", 2*60*60))
	validUserAt := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	validAssistantAt := time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		items       []any
		wantCreated []time.Time
	}{
		{
			name: "all missing",
			items: []any{
				map[string]any{"type": "userMessage", "id": "user", "content": "question"},
				map[string]any{"type": "agentMessage", "id": "assistant", "text": "answer"},
			},
			wantCreated: []time.Time{importedAt.UTC(), importedAt.UTC().Add(time.Nanosecond)},
		},
		{
			name: "mixed native and missing",
			items: []any{
				map[string]any{"type": "userMessage", "id": "user", "timestamp": validUserAt.Format(time.RFC3339Nano), "content": "question"},
				map[string]any{"type": "agentMessage", "id": "assistant-missing", "text": "intermediate"},
				map[string]any{"type": "agentMessage", "id": "assistant-native", "timestamp": validAssistantAt.Format(time.RFC3339Nano), "text": "answer"},
			},
			wantCreated: []time.Time{validUserAt, importedAt.UTC(), validAssistantAt},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := map[string]any{
				"id":        "thread-" + strings.ReplaceAll(tc.name, " ", "-"),
				"cwd":       t.TempDir(),
				"updatedAt": "2026-09-21T10:00:01Z",
				"turns":     []any{map[string]any{"items": tc.items}},
			}
			snapshot, err := codexNativeSnapshotAt(thread, importedAt)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(snapshot.Warnings, "\n"), codexNativeMissingTimestampWarning) {
				t.Fatalf("warnings = %+v", snapshot.Warnings)
			}
			if len(snapshot.Messages) != len(tc.wantCreated) {
				t.Fatalf("messages = %+v", snapshot.Messages)
			}
			for index, want := range tc.wantCreated {
				if got := snapshot.Messages[index].CreatedAt; !got.Equal(want) || got.IsZero() {
					t.Fatalf("message %d timestamp = %s, want %s", index, got, want)
				}
			}
		})
	}
}

// The fake checks the intent on disk before accepting a fork, and records every
// request across backend instances so retries cannot hide duplicate side effects.
func codexIntentFixture(t *testing.T, failure string) (string, string, NativeSessionPrepareOptions) {
	t.Helper()
	root := t.TempDir()
	fake, logPath := filepath.Join(root, "codex"), filepath.Join(root, "requests")
	opts := NativeSessionPrepareOptions{ImportID: uuid.NewString(), Handle: "source", Revision: "placeholder", DestinationDir: filepath.Join(root, "owned")}
	_, stage, err := nativePreparationPaths(opts, "codex")
	if err != nil {
		t.Fatal(err)
	}
	forkAction := `printf '{"id":%s,"result":{"thread":{"id":"owned"}}}\n' "$id"`
	if failure == "lost-reply" {
		forkAction = "exit 0"
	}
	ownedAction := `printf '{"id":%s,"result":{"thread":{"id":"owned","cwd":"/tmp","updatedAt":1,"turns":[]}}}\n' "$id"`
	if failure == "owned-read" {
		ownedAction = `printf '{"id":%s,"error":{"code":-1,"message":"owned read failed"}}\n' "$id"`
	}
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %q
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%%s,"result":{}}\n' "$id" ;;
    *'"method":"initialized"'*) : ;;
    *'"method":"thread/fork"'*)
      grep -q '"phase":"cloning"' %q || exit 81
      %s ;;
    *'"method":"thread/read"'*'"threadId":"source"'*) printf '{"id":%%s,"result":{"thread":{"id":"source","cwd":"/tmp","updatedAt":1,"turns":[]}}}\n' "$id" ;;
    *'"method":"thread/read"'*'"threadId":"owned"'*) %s ;;
    *) exit 82 ;;
  esac
done
`, logPath, nativePreparationIntentPath(stage), forkAction, ownedAction)
	writeTestExecutable(t, fake, []byte(script))
	backend, err := New("codex", Config{ExecutablePath: fake})
	if err != nil {
		t.Fatal(err)
	}
	source, err := backend.(NativeSessionProvider).ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: opts.Handle})
	if err != nil {
		t.Fatal(err)
	}
	opts.Revision = source.Summary.Revision
	return fake, logPath, opts
}

func codexIntentPrepare(t *testing.T, fake string, opts NativeSessionPrepareOptions) (PreparedNativeSession, error) {
	t.Helper()
	backend, err := New("codex", Config{ExecutablePath: fake})
	if err != nil {
		t.Fatal(err)
	}
	return backend.(NativeSessionImporter).PrepareNativeSession(context.Background(), opts)
}

func assertCodexForkCount(t *testing.T, logPath string, want int) {
	t.Helper()
	requests, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(requests), `"method":"thread/fork"`); got != want {
		t.Fatalf("fork count = %d, want %d: %s", got, want, requests)
	}
	if strings.Contains(string(requests), `"method":"thread/start"`) || strings.Contains(string(requests), `"method":"turn/start"`) {
		t.Fatalf("preparation started execution: %s", requests)
	}
}

func TestCodexNativeIntentRefusesUncertainForkRetry(t *testing.T) {
	for _, failure := range []string{"lost-reply", "owned-read"} {
		t.Run(failure, func(t *testing.T) {
			fake, logPath, opts := codexIntentFixture(t, failure)
			if _, err := codexIntentPrepare(t, fake, opts); err == nil {
				t.Fatal("expected interrupted preparation")
			} else if failure == "lost-reply" && nativeSessionErrorCode(err) != NativeSessionResumeUnavailable {
				t.Fatalf("uncertain fork error = %v", err)
			}
			_, stage, _ := nativePreparationPaths(opts, "codex")
			intent, exists, err := readNativePreparationIntent(nativePreparationIntentPath(stage))
			if err != nil || !exists || intent.Phase != "cloning" {
				t.Fatalf("intent = %+v, %v", intent, err)
			}
			if _, err := codexIntentPrepare(t, fake, opts); nativeSessionErrorCode(err) != NativeSessionResumeUnavailable {
				t.Fatalf("retry error = %v", err)
			}
			assertCodexForkCount(t, logPath, 1)
		})
	}
}

func TestCodexNativeIntentRecoversOwnedWithoutFork(t *testing.T) {
	fake, logPath, opts := codexIntentFixture(t, "")
	backend, err := New("codex", Config{ExecutablePath: fake})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := backend.(NativeSessionProvider).ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, stage, _ := nativePreparationPaths(opts, "codex")
	intent := nativePreparationIntent{ImportID: opts.ImportID, Provider: "codex", SourceHandle: opts.Handle, SourceRevision: opts.Revision, Phase: "owned", OwnedHandle: "owned", OwnedRevision: owned.Summary.Revision, ResumeSession: "owned", ResumeCwd: owned.Summary.Cwd}
	if err := writeNativeAtomicJSON(nativePreparationIntentPath(stage), intent); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		prepared, err := codexIntentPrepare(t, fake, opts)
		if err != nil || prepared.ResumeSessionID != "owned" || prepared.Snapshot.ResumeSessionID != "owned" {
			t.Fatalf("recovered = %+v, %v", prepared, err)
		}
	}
	manifest, exists, err := readNativePreparationManifest(manifestPath)
	if err != nil || !exists || manifest.OwnedRevision != owned.Summary.Revision {
		t.Fatalf("manifest = %+v, %v", manifest, err)
	}
	assertCodexForkCount(t, logPath, 0)
}

func TestCodexNativeIntentRejectsInvalidRecovery(t *testing.T) {
	for _, invalid := range []string{"corrupt", "mismatched", "unknown-phase", "incomplete-owned"} {
		t.Run(invalid, func(t *testing.T) {
			fake, logPath, opts := codexIntentFixture(t, "")
			_, stage, _ := nativePreparationPaths(opts, "codex")
			path := nativePreparationIntentPath(stage)
			intent := nativePreparationIntent{ImportID: opts.ImportID, Provider: "codex", SourceHandle: opts.Handle, SourceRevision: opts.Revision, Phase: "cloning"}
			switch invalid {
			case "mismatched":
				intent.SourceHandle = "other"
			case "unknown-phase":
				intent.Phase = "unexpected"
			case "incomplete-owned":
				intent.Phase = "owned"
			}
			if invalid == "corrupt" {
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := writeNativeAtomicJSON(path, intent); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(logPath)
			if _, err := codexIntentPrepare(t, fake, opts); err == nil {
				t.Fatal("invalid intent accepted")
			}
			after, _ := os.ReadFile(logPath)
			if string(after) != string(before) {
				t.Fatalf("invalid intent launched provider: %s", after)
			}
			assertCodexForkCount(t, logPath, 0)
		})
	}
}
