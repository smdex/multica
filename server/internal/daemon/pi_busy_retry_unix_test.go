//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestRunTaskPiBusyRetryDoesNotRetireHealthySession pins the distinction
// between a permanently rejected resume and a healthy transcript that another
// execution owns temporarily. The busy attempt must still fall back to a fresh
// session for this turn, but that fallback must not remove the original JSONL
// from every future session lookup.
func TestRunTaskPiBusyRetryDoesNotRetireHealthySession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	priorSessionID := filepath.Join(t.TempDir(), "busy-session.jsonl")
	claim, err := os.OpenFile(priorSessionID, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open prior session: %v", err)
	}
	defer claim.Close()
	if _, err := claim.WriteString("{}\n"); err != nil {
		t.Fatalf("seed prior session: %v", err)
	}
	if err := unix.Flock(int(claim.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock prior session: %v", err)
	}
	defer unix.Flock(int(claim.Fd()), unix.LOCK_UN) //nolint:errcheck -- best-effort test cleanup

	fakeBin := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":1}}}'
`
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		client:         NewClient(srv.URL),
		logger:         logger,
		workspaces:     make(map[string]*workspaceState),
		runtimeIndex:   map[string]Runtime{"rt-pi": {ID: "rt-pi", Provider: "pi"}},
		activeEnvRoots: make(map[string]int),
		cfg: Config{
			WorkspacesRoot: t.TempDir(),
			AgentTimeout:   5 * time.Second,
			ServerBaseURL:  srv.URL,
			Agents: map[string]AgentEntry{
				"pi": {Path: fakeBin},
			},
		},
	}
	task := Task{
		ID:             "task-pi-busy",
		WorkspaceID:    "ws-pi",
		RuntimeID:      "rt-pi",
		IssueID:        "issue-pi",
		AgentID:        "agent-pi",
		AuthToken:      "mat_pi_busy",
		PriorSessionID: priorSessionID,
		Agent: &AgentData{
			ID:   "agent-pi",
			Name: "pi-agent",
		},
	}

	result, err := d.runTask(context.Background(), task, "pi", 0, logger)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.Status != "completed" || result.Comment != "done" {
		t.Fatalf("result = %+v, want successful fresh-session retry", result)
	}
	if result.SessionID == "" || result.SessionID == priorSessionID {
		t.Fatalf("SessionID = %q, want a new session", result.SessionID)
	}
	if result.RetiredSessionID != "" {
		t.Fatalf("RetiredSessionID = %q, want empty for a temporarily busy healthy session", result.RetiredSessionID)
	}
}

func TestRunTaskRequireNativePiResumesOwnedForkAfterRestart(t *testing.T) {
	workspacesRoot := t.TempDir()
	originalCwd := t.TempDir()
	effectiveSessionRoot := filepath.Join(t.TempDir(), "pi-sessions")
	sourceSession := filepath.Join(effectiveSessionRoot, "project", "source.jsonl")
	const (
		importID = "018f0d34-9c36-7ace-86c2-8a6b13b5f004"
		agentID  = "018f0d34-9c36-7ace-86c2-8a6b13b5f005"
		chatID   = "018f0d34-9c36-7ace-86c2-8a6b13b5f006"
	)
	preparationDaemon := &Daemon{cfg: Config{WorkspacesRoot: workspacesRoot}}
	ownedDir, err := preparationDaemon.nativeImportDestination("rt-pi", importID)
	if err != nil {
		t.Fatalf("create owned Pi preparation directory: %v", err)
	}
	ownedSession := filepath.Join(ownedDir, "owned.jsonl")
	if err := os.MkdirAll(filepath.Dir(sourceSession), 0o700); err != nil {
		t.Fatalf("mkdir effective Pi session root: %v", err)
	}

	fixtureDir := t.TempDir()
	argsPath := filepath.Join(fixtureDir, "pi-args.txt")
	fakeBin := filepath.Join(fixtureDir, "pi")
	script := `#!/bin/sh
mode=""
session=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--mode" ]; then mode="$2"; shift 2; continue; fi
  if [ "$1" = "--session" ]; then session="$2"; shift 2; continue; fi
  shift
done
if [ "$mode" = "rpc" ]; then
  while IFS= read -r line; do
    id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    case "$line" in
      *'"type":"clone"'*)
        sed '1s/"id":"source-session"/"id":"owned-session"/' "$session" > "` + ownedSession + `"
        printf '{"id":"%s","type":"response","command":"clone","success":true,"data":{"cancelled":false}}\n' "$id"
        ;;
      *'"type":"get_state"'*)
        printf '{"id":"%s","type":"response","command":"get_state","success":true,"data":{"sessionFile":"` + ownedSession + `","sessionId":"owned-session","isStreaming":false}}\n' "$id"
        ;;
      *) exit 19 ;;
    esac
  done
  exit 0
fi
printf '%s\n' "$@" >> "` + argsPath + `"
printf '%s\n' "--session" >> "` + argsPath + `"
printf '%s\n' "$session" >> "` + argsPath + `"
printf '%s\n' "cwd=$PWD" >> "` + argsPath + `"
printf '%s\n' '--invocation-end--' >> "` + argsPath + `"
cat > /dev/null
printf '{"type":"message","id":"run-%s","parentId":"answer","timestamp":"2026-09-19T10:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"turn"}],"model":"gpt-test"}}\n' "$$" >> "$session"
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":1}}}'
`
	writeTestExecutable(t, fakeBin, []byte(script))
	sourceBytes := writePiNativeSession(t, sourceSession, originalCwd, "source-session")
	sourceSnapshot := readPiNativeSession(t, fakeBin, effectiveSessionRoot, sourceSession)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	config := Config{
		WorkspacesRoot: workspacesRoot,
		AgentTimeout:   5 * time.Second,
		ServerBaseURL:  srv.URL,
		Agents:         map[string]AgentEntry{"pi": {Path: fakeBin}},
	}

	// Import and execution happen in different daemon processes. The fake Pi
	// clone is in the daemon-owned preparation directory; a changed ambient
	// source root after preparation must not affect the strict execution path.
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", effectiveSessionRoot)
	importerDaemon := &Daemon{
		logger:       logger,
		runtimeIndex: map[string]Runtime{"rt-pi": {ID: "rt-pi", Provider: "pi"}},
		cfg:          config,
	}
	importBody, err := json.Marshal(workflowImportBody{
		ImportID:      importID,
		ChatSessionID: chatID,
		AgentID:       agentID,
		Handle:        sourceSnapshot.Summary.Handle,
		Revision:      sourceSnapshot.Summary.Revision,
		NativeID:      sourceSnapshot.Summary.NativeID,
	})
	if err != nil {
		t.Fatalf("encode native import command: %v", err)
	}
	importResult := importerDaemon.executeNativeSessionImport(context.Background(), protocol.AgentWorkflowCommand{
		ID: "request-pi-import", Kind: "native_session_import", RuntimeID: "rt-pi", Body: importBody,
	})
	if importResult.Status != "completed" {
		t.Fatalf("native import result = %+v", importResult)
	}
	var imported struct {
		OwnedNativeID string `json:"owned_native_id"`
	}
	if err := json.Unmarshal(importResult.Result, &imported); err != nil || imported.OwnedNativeID == "" {
		t.Fatalf("decode owned import result: %v %#v", err, importResult)
	}
	var rawImportResult map[string]json.RawMessage
	if err := json.Unmarshal(importResult.Result, &rawImportResult); err != nil {
		t.Fatalf("decode raw import result: %v", err)
	}
	for _, field := range []string{"chat_session_id", "agent_id"} {
		if _, found := rawImportResult[field]; found {
			t.Fatalf("daemon import result leaked server-owned %s", field)
		}
	}
	ownedSnapshot := readOwnedPiNativeSession(t, fakeBin, ownedDir, ownedSession, imported.OwnedNativeID)
	if ownedSnapshot.Summary.NativeID == sourceSnapshot.Summary.NativeID {
		t.Fatal("native import did not create a distinct owned Pi identity")
	}
	if sourceAfter, err := os.ReadFile(sourceSession); err != nil || string(sourceAfter) != sourceBytes {
		t.Fatalf("native preparation changed source Pi session: %v %q", err, sourceAfter)
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", filepath.Join(t.TempDir(), "ambient-session-root"))
	newDaemon := func() *Daemon {
		return &Daemon{
			client:         NewClient(srv.URL),
			logger:         logger,
			workspaces:     make(map[string]*workspaceState),
			runtimeIndex:   map[string]Runtime{"rt-pi": {ID: "rt-pi", Provider: "pi"}},
			activeEnvRoots: make(map[string]int),
			localPathLocks: NewLocalPathLocker(),
			cfg:            config,
		}
	}
	d := newDaemon()
	task := Task{
		ID:             "task-pi-native-resume",
		WorkspaceID:    "ws-pi",
		RuntimeID:      "rt-pi",
		IssueID:        "issue-pi",
		AgentID:        agentID,
		ChatSessionID:  chatID,
		AuthToken:      "mat_pi_native_resume",
		PriorSessionID: ownedSession,
		PriorWorkDir:   originalCwd,
		ResumePolicy:   "require_native",
		Agent: &AgentData{
			ID:        agentID,
			Name:      "pi-agent",
			CustomEnv: map[string]string{"PI_CODING_AGENT_SESSION_DIR": effectiveSessionRoot},
		},
	}

	result, err := d.runTask(context.Background(), task, "pi", 0, logger)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.Status != "completed" || result.SessionID != ownedSession {
		t.Fatalf("result = %+v, want completed run on owned session %q", result, ownedSession)
	}
	afterFirst, err := d.loadNativeImportContext(task.RuntimeID, ownedSession)
	if err != nil {
		t.Fatalf("load first native revision: %v", err)
	}
	if afterFirst.OwnedRevision == ownedSnapshot.Summary.Revision {
		t.Fatal("first owned execution did not advance the durable native revision")
	}
	if afterFirst.DestinationDir != ownedDir || afterFirst.AgentID != agentID || afterFirst.ChatSessionID != chatID {
		t.Fatalf("durable native context lost its owned binding: %+v", afterFirst)
	}

	// A new daemon must accept the revision written by the first owned turn and
	// resume that exact fork again, never a fresh Pi session.
	d = newDaemon()
	task.ID = "task-pi-native-resume-second"
	result, err = d.runTask(context.Background(), task, "pi", 0, logger)
	if err != nil {
		t.Fatalf("second runTask after restart: %v", err)
	}
	if result.Status != "completed" || result.SessionID != ownedSession {
		t.Fatalf("second result = %+v, want same owned session %q", result, ownedSession)
	}
	afterSecond, err := d.loadNativeImportContext(task.RuntimeID, ownedSession)
	if err != nil {
		t.Fatalf("load second native revision: %v", err)
	}
	if afterSecond.OwnedRevision == afterFirst.OwnedRevision {
		t.Fatal("second owned execution did not advance the durable native revision")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake Pi invocation: %v", err)
	}
	if strings.Count(string(args), "--invocation-end--") != 2 {
		t.Fatalf("Pi launch count = %d, want two strict native launches:\n%s", strings.Count(string(args), "--invocation-end--"), args)
	}
	if !strings.Contains(string(args), "--session\n"+ownedSession+"\n") {
		t.Fatalf("Pi did not receive the persisted owned session:\n%s", args)
	}
	if !strings.Contains(string(args), "cwd="+originalCwd+"\n") {
		t.Fatalf("Pi cwd did not retain the validated original cwd:\n%s", args)
	}
	if sourceAfter, err := os.ReadFile(sourceSession); err != nil || string(sourceAfter) != sourceBytes {
		t.Fatalf("strict owned execution changed source Pi session: %v %q", err, sourceAfter)
	}

	// A foreign write after the daemon's last acknowledged revision must block
	// before a third provider launch. It cannot be reclassified as this
	// daemon's own append after a restart.
	foreign, err := os.OpenFile(ownedSession, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open owned session for foreign mutation: %v", err)
	}
	if _, err := foreign.WriteString(`{"type":"foreign"}` + "\n"); err != nil {
		_ = foreign.Close()
		t.Fatalf("append foreign owned-session mutation: %v", err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatalf("close foreign owned-session mutation: %v", err)
	}
	d = newDaemon()
	task.ID = "task-pi-native-external-write"
	if _, err := d.runTask(context.Background(), task, "pi", 0, logger); err == nil {
		t.Fatal("strict native task ran after an external owned-session mutation")
	}
	args, err = os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake Pi invocation after external mutation: %v", err)
	}
	if strings.Count(string(args), "--invocation-end--") != 2 {
		t.Fatalf("external mutation reached Pi execution instead of failing closed:\n%s", args)
	}
}

func TestRunTaskRequireNativePiDoesNotRetryFreshAfterResumeRefusal(t *testing.T) {
	workspacesRoot := t.TempDir()
	originalCwd := t.TempDir()
	effectiveSessionRoot := filepath.Join(t.TempDir(), "pi-sessions")
	ownedSession := filepath.Join(effectiveSessionRoot, "project", "owned.jsonl")
	if err := os.MkdirAll(filepath.Dir(ownedSession), 0o700); err != nil {
		t.Fatalf("mkdir effective Pi session root: %v", err)
	}
	fixtureDir := t.TempDir()
	argsPath := filepath.Join(fixtureDir, "pi-args.txt")
	fakeBin := filepath.Join(fixtureDir, "pi")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "` + argsPath + `"
printf '%s\n' '--invocation-end--' >> "` + argsPath + `"
cat > /dev/null
printf '%s\n' 'Stored session working directory does not exist' >&2
exit 1
`
	writeTestExecutable(t, fakeBin, []byte(script))
	writePiNativeSession(t, ownedSession, originalCwd, "owned-session")
	ownedSnapshot := readPiNativeSession(t, fakeBin, effectiveSessionRoot, ownedSession)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	config := Config{
		WorkspacesRoot: workspacesRoot,
		AgentTimeout:   5 * time.Second,
		ServerBaseURL:  srv.URL,
		Agents:         map[string]AgentEntry{"pi": {Path: fakeBin}},
	}
	importerDaemon := &Daemon{cfg: config}
	if err := importerDaemon.persistNativeImportContext(nativeImportContext{
		Version:         1,
		RuntimeID:       "rt-pi",
		ImportID:        "018f0d34-9c36-7ace-86c2-8a6b13b5f005",
		ChatSessionID:   "chat-pi",
		AgentID:         "agent-pi",
		SourceNativeID:  "source-native",
		OwnedNativeID:   ownedSnapshot.Summary.NativeID,
		OwnedHandle:     ownedSnapshot.Summary.Handle,
		OwnedRevision:   ownedSnapshot.Summary.Revision,
		DestinationDir:  effectiveSessionRoot,
		ResumeSessionID: ownedSession,
		ResumeCwd:       originalCwd,
	}); err != nil {
		t.Fatalf("persist native import context: %v", err)
	}
	d := &Daemon{
		client:         NewClient(srv.URL),
		logger:         logger,
		workspaces:     make(map[string]*workspaceState),
		runtimeIndex:   map[string]Runtime{"rt-pi": {ID: "rt-pi", Provider: "pi"}},
		activeEnvRoots: make(map[string]int),
		localPathLocks: NewLocalPathLocker(),
		cfg:            config,
	}
	task := Task{
		ID:             "task-pi-native-refusal",
		WorkspaceID:    "ws-pi",
		RuntimeID:      "rt-pi",
		IssueID:        "issue-pi",
		AgentID:        "agent-pi",
		ChatSessionID:  "chat-pi",
		AuthToken:      "mat_pi_native_refusal",
		PriorSessionID: ownedSession,
		PriorWorkDir:   originalCwd,
		ResumePolicy:   "require_native",
		Agent: &AgentData{ID: "agent-pi", Name: "pi-agent", CustomEnv: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": effectiveSessionRoot,
		}},
	}

	result, err := d.runTask(context.Background(), task, "pi", 0, logger)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.Status != "blocked" {
		t.Fatalf("result = %+v, want strict native resume failure", result)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake Pi invocation: %v", err)
	}
	if strings.Count(string(args), "--invocation-end--") != 1 {
		t.Fatalf("Pi launch count = %d, want no fresh-session retry after strict resume refusal:\n%s", strings.Count(string(args), "--invocation-end--"), args)
	}
	if !strings.Contains(string(args), "--session\n"+ownedSession+"\n") {
		t.Fatalf("strict launch did not use the owned session:\n%s", args)
	}
}

func writePiNativeSession(t *testing.T, path, cwd, id string) string {
	t.Helper()
	data := strings.Join([]string{
		`{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-09-19T10:00:00Z","cwd":"` + cwd + `"}`,
		`{"type":"message","id":"root-user","parentId":null,"timestamp":"2026-09-19T10:00:01Z","message":{"role":"user","content":"root prompt"}}`,
		`{"type":"message","id":"answer","parentId":"root-user","timestamp":"2026-09-19T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"answer"}],"model":"gpt-test"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write Pi native session: %v", err)
	}
	return data
}

func readPiNativeSession(t *testing.T, fakeBin, sessionRoot, ownedSession string) agent.NativeSessionSnapshot {
	t.Helper()
	backend, err := agent.New("pi", agent.Config{
		ExecutablePath: fakeBin,
		Env:            map[string]string{"PI_CODING_AGENT_SESSION_DIR": sessionRoot},
	})
	if err != nil {
		t.Fatalf("new fake Pi backend: %v", err)
	}
	provider, ok := backend.(agent.NativeSessionProvider)
	if !ok {
		t.Fatal("Pi backend does not expose native-session reads")
	}
	snapshot, err := provider.ReadNativeSession(context.Background(), agent.NativeSessionReadOptions{Handle: ownedSession})
	if err != nil {
		t.Fatalf("read fake Pi native session: %v", err)
	}
	return snapshot
}

func readOwnedPiNativeSession(t *testing.T, fakeBin, destinationDir, ownedSession, nativeID string) agent.NativeSessionSnapshot {
	t.Helper()
	backend, err := agent.New("pi", agent.Config{ExecutablePath: fakeBin})
	if err != nil {
		t.Fatalf("new fake Pi backend: %v", err)
	}
	provider, ok := backend.(agent.NativeOwnedSessionProvider)
	if !ok {
		t.Fatal("Pi backend does not expose owned-native-session reads")
	}
	snapshot, err := provider.ReadOwnedNativeSession(context.Background(), agent.NativeOwnedSessionReadOptions{
		NativeSessionReadOptions: agent.NativeSessionReadOptions{Handle: ownedSession},
		DestinationDir:           destinationDir,
		NativeID:                 nativeID,
	})
	if err != nil {
		t.Fatalf("read fake owned Pi native session: %v", err)
	}
	return snapshot
}
