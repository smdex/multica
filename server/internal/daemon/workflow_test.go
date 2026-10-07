package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type workflowLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *workflowLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *workflowLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestClientSendHeartbeat_ReportsWorkflowCapabilities(t *testing.T) {
	want := &protocol.AgentWorkflowCapabilities{
		NativeSessions: protocol.AgentWorkflowNativeSessionCapabilities{List: true, Import: true},
		Controls:       protocol.AgentWorkflowControlCapabilities{Steer: true, Approvals: true, Questions: true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RuntimeID                 string                              `json:"runtime_id"`
			AgentWorkflowCapabilities *protocol.AgentWorkflowCapabilities `json:"agent_workflow_capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode heartbeat: %v", err)
		}
		if body.RuntimeID != "runtime" || body.AgentWorkflowCapabilities == nil || *body.AgentWorkflowCapabilities != *want {
			t.Fatalf("heartbeat = %#v", body)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL).SendHeartbeat(context.Background(), "runtime", want); err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
}

func TestAgentWorkflowCapabilities_AdvertiseIdleRuntimeInteractionSupport(t *testing.T) {
	missingExecutable := filepath.Join(t.TempDir(), "missing-pi")
	d := &Daemon{
		cfg:          Config{Agents: map[string]AgentEntry{"pi": {Path: missingExecutable}}},
		runtimeIndex: map[string]Runtime{"runtime-pi": {ID: "runtime-pi", Provider: "pi"}},
	}

	// ResolveBackend only constructs the provider. This path must not resolve
	// or start the configured executable just to advertise idle chat support.
	caps := d.agentWorkflowCapabilities("runtime-pi")
	if caps == nil {
		t.Fatal("idle Pi runtime did not advertise workflow capabilities")
	}
	if got, want := caps.Controls, (protocol.AgentWorkflowControlCapabilities{Steer: true, Approvals: true, Questions: true}); got != want {
		t.Fatalf("idle Pi controls = %+v, want %+v", got, want)
	}
	if !d.supportsWorkflowChat("runtime-pi") {
		t.Fatal("idle Pi runtime did not admit chat from its declared interaction capabilities")
	}
}

func TestRunTaskWorkflowStartClaimRejected(t *testing.T) {
	for _, mode := range []string{"chat", "autonomous"} {
		t.Run(mode, func(t *testing.T) {
			var d *Daemon
			var startCalled atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/start") {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				startCalled.Store(true)
				var start struct {
					RunID        string   `json:"run_id"`
					RuntimeID    string   `json:"runtime_id"`
					DispatchedAt string   `json:"dispatched_at"`
					Capabilities []string `json:"capabilities"`
				}
				if err := json.NewDecoder(r.Body).Decode(&start); err != nil {
					t.Errorf("decode start request: %v", err)
				}
				if start.RunID == "" || start.RuntimeID != "runtime-1" || start.DispatchedAt != startTestClaim().DispatchedAt {
					t.Errorf("start request lost claim or run identity: %+v", start)
				}
				run := d.workflowRun("task-1", start.RunID, start.RuntimeID)
				if (run != nil) != (mode == "chat") {
					t.Errorf("workflow registration for %s = %v", mode, run)
				}
				if d.workflowRun("task-1", "different-run", start.RuntimeID) != nil || d.workflowRun("task-1", start.RunID, "different-runtime") != nil {
					t.Error("workflow registry admitted a mismatched run or runtime")
				}
				if mode == "autonomous" && (len(start.Capabilities) != 1 || start.Capabilities[0] != protocol.DaemonCapabilityTaskSupplementV1) {
					t.Errorf("start request lost supplement capability offer: %+v", start)
				}
				http.Error(w, "claim is stale", http.StatusConflict)
			}))
			defer srv.Close()
			// A custom profile bypasses executable discovery. The start rejection
			// must return before attempting to launch this missing test executable.
			d = &Daemon{
				client:       NewClient(srv.URL),
				logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
				workspaces:   make(map[string]*workspaceState),
				runtimeIndex: map[string]Runtime{"runtime-1": {ID: "runtime-1", Provider: "claude", ProfileID: "profile-1"}},
				profileLaunchSpecs: map[string]profileLaunchSpec{
					"profile-1": {path: filepath.Join(t.TempDir(), "missing-claude"), version: "2.1.110"},
				},
				activeEnvRoots: make(map[string]int),
				cfg:            Config{WorkspacesRoot: t.TempDir()},
			}
			claim := startTestClaim()
			claim.WorkspaceID = "workspace-1"
			claim.AgentID = "agent-1"
			claim.Agent = &AgentData{ID: "agent-1", Name: "test-agent"}
			claim.InteractionMode = mode
			if mode == "chat" {
				claim.ChatSessionID = "chat-1"
			} else {
				claim.IssueID = "issue-1"
			}
			_, err := d.runTask(context.Background(), claim, "claude", 0, d.logger)
			if !startCalled.Load() || !errors.Is(err, errStartClaimRejected) {
				t.Fatalf("start called=%t error=%v, want rejected start claim", startCalled.Load(), err)
			}
			d.workflowMu.RLock()
			remaining := len(d.workflowRuns)
			d.workflowMu.RUnlock()
			if remaining != 0 {
				t.Fatalf("rejected start left %d registered workflow runs", remaining)
			}
		})
	}
}

func TestWorkflowRunReportLoop_ClosedStaleReportStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/tasks/task/controls" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		http.Error(w, "run no longer exists", http.StatusConflict)
	}))
	defer srv.Close()

	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	run := &workflowRun{
		daemon: d, taskID: "task", runID: "run",
		pending: make(map[string]agent.InteractionRequest), resolving: make(map[string]string), expiring: make(map[string]bool),
		wake: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() { run.reportLoop(); close(done) }()
	run.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed stale live report retried instead of terminating")
	}
}

func TestWorkflowRunRespond_RejectedKeepsInteractionPending(t *testing.T) {
	request := agent.InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "question"}
	run := &workflowRun{
		session: &agent.Session{RespondToInteraction: func(context.Context, agent.InteractionResponse) (agent.InputDelivery, error) {
			return agent.InputDelivery{State: "rejected", Code: "provider_busy"}, nil
		}},
		pending: map[string]agent.InteractionRequest{request.ID: request}, resolving: make(map[string]string), expiring: make(map[string]bool),
	}
	delivery, err := run.respond(context.Background(), agent.InteractionResponse{ID: "command", InteractionID: request.ID, ExpectedTurnID: request.TurnID})
	if err != nil || delivery.State != "rejected" {
		t.Fatalf("delivery = %#v, err = %v", delivery, err)
	}
	if _, exists := run.pending[request.ID]; !exists {
		t.Fatal("definitively rejected response removed the still-pending interaction")
	}
	if len(run.resolving) != 0 {
		t.Fatalf("resolving entries left behind: %#v", run.resolving)
	}
}

func TestWorkflowRunRespond_UnknownDoesNotReplayExpiry(t *testing.T) {
	request := agent.InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "approval"}
	calls := 0
	run := &workflowRun{
		session: &agent.Session{RespondToInteraction: func(context.Context, agent.InteractionResponse) (agent.InputDelivery, error) {
			calls++
			return agent.InputDelivery{State: "unknown", Code: "connection_lost"}, nil
		}},
		pending: map[string]agent.InteractionRequest{request.ID: request}, resolving: make(map[string]string), expiring: make(map[string]bool),
	}
	delivery, err := run.respond(context.Background(), agent.InteractionResponse{ID: "command", InteractionID: request.ID, ExpectedTurnID: request.TurnID})
	if err != nil || delivery.State != "unknown" {
		t.Fatalf("delivery = %#v, err = %v", delivery, err)
	}
	run.expireInteraction(request.ID, time.Now().Add(-time.Second))
	if calls != 1 {
		t.Fatalf("provider interaction calls = %d, want one uncertain delivery only", calls)
	}
	if _, exists := run.pending[request.ID]; exists {
		t.Fatal("unknown delivery left an interaction eligible for expiry replay")
	}
}

func TestNativeImportContextRoundTrip(t *testing.T) {
	d := &Daemon{cfg: Config{WorkspacesRoot: t.TempDir()}}
	record := nativeImportContext{
		Version: 1, RuntimeID: "runtime", ImportID: "018f0d34-9c36-7ace-86c2-8a6b13b5f004",
		ChatSessionID: "chat", AgentID: "agent", SourceNativeID: "source", OwnedNativeID: "owned", OwnedHandle: "owned-handle", OwnedRevision: "owned-revision", DestinationDir: t.TempDir(),
		ProviderHome: t.TempDir(), ResumeSessionID: "resume", ResumeCwd: t.TempDir(),
	}
	if err := d.persistNativeImportContext(record); err != nil {
		t.Fatalf("persist: %v", err)
	}
	got, err := d.loadNativeImportContext(record.RuntimeID, record.ResumeSessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != record {
		t.Fatalf("context = %#v, want %#v", got, record)
	}

	record.ProviderHome = "relative-codex-home"
	if err := d.persistNativeImportContext(record); err == nil {
		t.Fatal("persisted native import context accepted a relative Codex provider home")
	}
}

func TestNativeImportContextKeepsCodexHomeAfterAmbientDrift(t *testing.T) {
	root := t.TempDir()
	sourceHome := filepath.Join(root, "canonical-source-home")
	if err := os.Mkdir(sourceHome, 0o700); err != nil {
		t.Fatalf("create source home: %v", err)
	}
	alias := filepath.Join(root, "source-home-alias")
	if err := os.Symlink(sourceHome, alias); err != nil {
		t.Skipf("create CODEX_HOME symlink: %v", err)
	}
	t.Setenv("CODEX_HOME", alias)
	providerHome, err := nativeImportProviderHome("codex")
	if err != nil {
		t.Fatalf("resolve import-time Codex home: %v", err)
	}
	if providerHome != sourceHome {
		t.Fatalf("provider home = %q, want %q", providerHome, sourceHome)
	}
	d := &Daemon{cfg: Config{WorkspacesRoot: t.TempDir()}}
	record := nativeImportContext{
		Version: 1, RuntimeID: "runtime", ImportID: "018f0d34-9c36-7ace-86c2-8a6b13b5f004",
		ChatSessionID: "chat", AgentID: "agent", SourceNativeID: "source", OwnedNativeID: "owned", OwnedHandle: "owned-handle", OwnedRevision: "owned-revision", DestinationDir: t.TempDir(),
		ProviderHome: providerHome, ResumeSessionID: "resume", ResumeCwd: t.TempDir(),
	}
	if err := d.persistNativeImportContext(record); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// A new daemon process may resolve a different ambient home. Strict resume
	// consumes the persisted source identity instead.
	t.Setenv("CODEX_HOME", t.TempDir())
	loaded, err := d.loadNativeImportContext(record.RuntimeID, record.ResumeSessionID)
	if err != nil {
		t.Fatalf("load after ambient drift: %v", err)
	}
	if loaded.ProviderHome != sourceHome {
		t.Fatalf("persisted provider home = %q, want %q", loaded.ProviderHome, sourceHome)
	}
}

func TestValidateNativeImportTaskBinding(t *testing.T) {
	record := nativeImportContext{AgentID: "agent-a", ChatSessionID: "chat-a"}
	for _, tc := range []struct {
		name string
		task Task
		want bool
	}{
		{name: "matching reservation", task: Task{AgentID: "agent-a", ChatSessionID: "chat-a"}},
		{name: "different agent", task: Task{AgentID: "agent-b", ChatSessionID: "chat-a"}, want: true},
		{name: "different chat", task: Task{AgentID: "agent-a", ChatSessionID: "chat-b"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateNativeImportTaskBinding(tc.task, record); (err != nil) != tc.want {
				t.Fatalf("validateNativeImportTaskBinding() error = %v, want error %t", err, tc.want)
			}
		})
	}
}

func TestWorkflowReportReplayLoop_RetriesPersistedResultAfterRestart(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/runtimes/runtime/agent-workflow-requests/request/result" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := Config{WorkspacesRoot: t.TempDir(), ServerBaseURL: srv.URL, DaemonID: "daemon"}
	store := newWorkflowReportStore(cfg)
	record := workflowReportRecord{Version: 1, RuntimeID: "runtime", RequestID: "request", Result: workflowCompleted(map[string]string{"ok": "yes"})}
	if err := store.enqueue(record); err != nil {
		t.Fatalf("enqueue before restart: %v", err)
	}
	// Construct a new daemon over the existing outbox. The only retry signal
	// after the initial 503 is the replay ticker, proving a process restart
	// does not need another server heartbeat to finish delivery.
	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workflowReports: newWorkflowReportStore(cfg), workflowReportWakeup: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.workflowReportReplayLoop(ctx)

	deadline := time.After(3 * time.Second)
	for {
		reports, err := d.workflowReports.list()
		if err != nil {
			t.Fatalf("list reports: %v", err)
		}
		if len(reports) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("persisted report still queued after %d attempts", calls.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if calls.Load() < 2 {
		t.Fatalf("delivery calls = %d, want failed initial call plus ticker retry", calls.Load())
	}
}

func TestWorkflowReportReplayLoop_DeliversValidReportsAlongsideInvalidArtifacts(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/runtimes/runtime/agent-workflow-requests/valid/result" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		delivered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := Config{WorkspacesRoot: t.TempDir(), ServerBaseURL: srv.URL, DaemonID: "daemon"}
	store := newWorkflowReportStore(cfg)
	valid := workflowReportRecord{Version: 1, RuntimeID: "runtime", RequestID: "valid", Result: workflowCompleted(map[string]string{"ok": "yes"})}
	if err := store.enqueue(valid); err != nil {
		t.Fatalf("enqueue valid report: %v", err)
	}
	malformedPath := filepath.Join(store.dir, "malformed.json")
	if err := os.WriteFile(malformedPath, []byte(`{`), 0o600); err != nil {
		t.Fatalf("write malformed report: %v", err)
	}
	mismatched := workflowReportRecord{Version: 1, RuntimeID: "runtime", RequestID: "wrong", Result: workflowCompleted(map[string]string{"ok": "no"})}
	body, err := json.Marshal(mismatched)
	if err != nil {
		t.Fatalf("marshal mismatched report: %v", err)
	}
	mismatchedPath := filepath.Join(store.dir, workflowReportFileName("runtime", "mismatched"))
	if err := os.WriteFile(mismatchedPath, body, 0o600); err != nil {
		t.Fatalf("write mismatched report: %v", err)
	}

	reports, err := store.list()
	if err == nil || !strings.Contains(err.Error(), "decode workflow report malformed.json") || !strings.Contains(err.Error(), "does not match runtime/request identity") {
		t.Fatalf("list error = %v", err)
	}
	if len(reports) != 1 || !reflect.DeepEqual(reports[0], valid) {
		t.Fatalf("list reports = %#v, want only %#v", reports, valid)
	}

	var logs workflowLogBuffer
	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(&logs, nil)), workflowReports: store, workflowReportWakeup: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.workflowReportReplayLoop(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("workflow report replay loop did not stop")
		}
	})

	deadline := time.After(3 * time.Second)
	for {
		reports, listErr := store.list()
		if delivered.Load() == 1 && len(reports) == 0 && listErr != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("valid report was not replayed while invalid artifacts remained: delivered=%d reports=%#v error=%v", delivered.Load(), reports, listErr)
		case <-time.After(20 * time.Millisecond):
		}
	}
	for _, path := range []string{malformedPath, mismatchedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("invalid report artifact %s was not preserved: %v", path, err)
		}
	}
	if !strings.Contains(logs.String(), "load pending workflow reports; preserving invalid artifacts") {
		t.Fatalf("invalid report artifacts were not logged: %s", logs.String())
	}
}

func TestWorkflowReportReplayLoop_LogsAcknowledgementFailure(t *testing.T) {
	var logs workflowLogBuffer
	cfg := Config{WorkspacesRoot: t.TempDir(), ServerBaseURL: "http://unused", DaemonID: "daemon"}
	store := newWorkflowReportStore(cfg)
	record := workflowReportRecord{Version: 1, RuntimeID: "runtime", RequestID: "request", Result: workflowCompleted(map[string]string{"ok": "yes"})}
	if err := store.enqueue(record); err != nil {
		t.Fatalf("enqueue report: %v", err)
	}
	reportPath := filepath.Join(store.dir, workflowReportFileName(record.RuntimeID, record.RequestID))
	keptPath := filepath.Join(reportPath, "kept")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(reportPath); err != nil {
			t.Fatalf("remove report before acknowledgement: %v", err)
		}
		if err := os.Mkdir(reportPath, 0o700); err != nil {
			t.Fatalf("replace report with directory: %v", err)
		}
		if err := os.WriteFile(keptPath, []byte("preserve"), 0o600); err != nil {
			t.Fatalf("retain acknowledgement failure artifact: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(&logs, nil)), workflowReports: store, workflowReportWakeup: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.workflowReportReplayLoop(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("workflow report replay loop did not stop")
		}
	})

	deadline := time.After(3 * time.Second)
	for !strings.Contains(logs.String(), "acknowledge replayed workflow report; retaining for retry") {
		select {
		case <-deadline:
			t.Fatalf("acknowledgement failure was not logged: %s", logs.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, err := os.Stat(keptPath); err != nil {
		t.Fatalf("acknowledgement failure did not retain its artifact: %v", err)
	}
}

func TestWorkflowDeliveryResult_UnknownKeepsDeliveryResult(t *testing.T) {
	result := workflowDeliveryResult(agent.InputDelivery{State: "unknown", Code: "connection_lost"}, nil)
	if result.Status != "unknown" || result.Error == nil || result.Error.Code != "connection_lost" {
		t.Fatalf("result = %#v", result)
	}
	if string(result.Result) != `{"delivery":"unknown","code":"connection_lost"}` {
		t.Fatalf("delivery result = %s", result.Result)
	}
}
