package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	workflowContractTaskID = "00000000-0000-0000-0000-000000000101"
	workflowContractRunID  = "00000000-0000-0000-0000-000000000102"
)

func TestWorkflowContract_AdmissionNormalizesZeroDeadlineAndPreservesRetryPayload(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
		second = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/tasks/task/controls" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/api/daemon/tasks/task/interactions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read report: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		count := len(bodies)
		mu.Unlock()
		if count == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		close(second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	run := &workflowRun{
		daemon: d, taskID: "task", runID: "run",
		session: &agent.Session{RespondToInteraction: func(context.Context, agent.InteractionResponse) (agent.InputDelivery, error) {
			return agent.InputDelivery{State: "accepted"}, nil
		}},
		pending: make(map[string]agent.InteractionRequest), resolving: make(map[string]string), expiring: make(map[string]bool),
		wake: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() { run.reportLoop(); close(done) }()

	admittedAt := time.Now()
	run.presentInteraction(agent.InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "approval", Title: "Allow", Choices: []agent.InteractionChoice{{ID: "allow_once", Label: "Allow"}}})
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("interaction was not retried after the transient report failure")
	}

	mu.Lock()
	gotBodies := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(gotBodies) != 2 {
		t.Fatalf("reports = %d, want two", len(gotBodies))
	}
	if !bytes.Equal(gotBodies[0], gotBodies[1]) {
		t.Fatalf("retry changed interaction report\nfirst:  %s\nsecond: %s", gotBodies[0], gotBodies[1])
	}
	var report struct {
		RunID       string                                   `json:"run_id"`
		Interaction protocol.AgentWorkflowInteractionRequest `json:"interaction"`
	}
	if err := json.Unmarshal(gotBodies[0], &report); err != nil {
		t.Fatalf("decode daemon report: %v", err)
	}
	if report.RunID != "run" || report.Interaction.ID != "interaction" {
		t.Fatalf("report = %#v", report)
	}
	if report.Interaction.ExpiresAt.IsZero() {
		t.Fatal("daemon forwarded the adapter's missing interaction deadline")
	}
	if got, want := report.Interaction.ExpiresAt.Sub(admittedAt), 15*time.Minute; got < want-time.Second || got > want+time.Second {
		t.Fatalf("normalized deadline offset = %s, want %s", got, want)
	}

	run.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("report loop did not stop")
	}
}

func TestWorkflowContract_ActualAdapterInteractionsReachDaemonHTTP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("adapter protocol fixtures use POSIX shell executables")
	}
	payloads := make(map[string]json.RawMessage)
	for _, provider := range []string{"codex", "claude", "pi"} {
		t.Run(provider, func(t *testing.T) {
			request, session, cancel := workflowContractAdapterInteraction(t, provider)
			defer cancel()
			if !request.ExpiresAt.IsZero() {
				t.Fatalf("%s adapter unexpectedly supplied deadline %s", provider, request.ExpiresAt)
			}

			type interactionReport struct {
				wire        json.RawMessage
				interaction protocol.AgentWorkflowInteractionRequest
			}
			reports := make(chan interactionReport, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/daemon/tasks/" + workflowContractTaskID + "/interactions":
					wire, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatalf("read %s interaction report: %v", provider, err)
					}
					var body struct {
						RunID       string                                   `json:"run_id"`
						Interaction protocol.AgentWorkflowInteractionRequest `json:"interaction"`
					}
					if err := json.Unmarshal(wire, &body); err != nil {
						t.Fatalf("decode %s interaction report: %v", provider, err)
					}
					if body.RunID != workflowContractRunID {
						t.Fatalf("%s run ID = %q", provider, body.RunID)
					}
					reports <- interactionReport{wire: wire, interaction: body.Interaction}
				case "/api/daemon/tasks/" + workflowContractTaskID + "/controls":
				default:
					t.Fatalf("%s unexpected daemon report path %q", provider, r.URL.Path)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			run := newWorkflowContractRun(NewClient(srv.URL), session)
			done := make(chan struct{})
			go func() { run.reportLoop(); close(done) }()
			admittedAt := time.Now()
			run.presentInteraction(request)
			select {
			case report := <-reports:
				if report.interaction.ID != request.ID || report.interaction.ExpiresAt.IsZero() {
					t.Fatalf("%s daemon interaction report = %#v", provider, report.interaction)
				}
				if got, want := report.interaction.ExpiresAt.Sub(admittedAt), workflowInteractionDeadline; got < want-time.Second || got > want+time.Second {
					t.Fatalf("%s normalized deadline offset = %s, want %s", provider, got, want)
				}
				payloads[provider] = append(json.RawMessage(nil), report.wire...)
			case <-time.After(3 * time.Second):
				t.Fatalf("%s adapter interaction did not reach daemon HTTP reporting", provider)
			}

			ctx, responseCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := session.RespondToInteraction(ctx, workflowContractInteractionResponse(request))
			responseCancel()
			if err != nil {
				t.Fatalf("resolve %s provider interaction: %v", provider, err)
			}
			select {
			case result, ok := <-session.Result:
				if !ok || result.Status != "completed" {
					t.Fatalf("%s provider result = %#v, open=%t", provider, result, ok)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s provider did not settle after its interaction response", provider)
			}

			run.close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatalf("%s daemon report loop did not stop", provider)
			}
		})
	}
	fixture, err := json.MarshalIndent(struct {
		TaskID             string                     `json:"task_id"`
		InteractionReports map[string]json.RawMessage `json:"interaction_reports"`
	}{TaskID: workflowContractTaskID, InteractionReports: payloads}, "", "  ")
	if err != nil {
		t.Fatalf("encode adapter interaction fixture: %v", err)
	}
	const fixturePath = "/tmp/multica-daemon-adapter-interaction-fixtures.json"
	if err := os.WriteFile(fixturePath, append(fixture, '\n'), 0o600); err != nil {
		t.Fatalf("write adapter interaction fixture: %v", err)
	}
	t.Logf("wrote actual adapter interaction reports to %s", fixturePath)
}

func newWorkflowContractRun(client *Client, session *agent.Session) *workflowRun {
	return &workflowRun{
		daemon: &Daemon{client: client, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, taskID: workflowContractTaskID, runID: workflowContractRunID, session: session,
		pending: make(map[string]agent.InteractionRequest), resolving: make(map[string]string), expiring: make(map[string]bool), wake: make(chan struct{}, 1),
	}
}

func workflowContractAdapterInteraction(t *testing.T, provider string) (agent.InteractionRequest, *agent.Session, context.CancelFunc) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), provider)
	workflowWriteFakeExecutable(t, fake, workflowContractAdapterScript(provider))
	options := agent.ExecOptions{InteractionMode: "chat", Timeout: 5 * time.Second}
	if provider == "pi" {
		sessionPath := filepath.Join(t.TempDir(), "pi-session.jsonl")
		if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write Pi session: %v", err)
		}
		options.ResumeSessionID = sessionPath
	}
	backend, err := agent.New(provider, agent.Config{ExecutablePath: fake, Env: map[string]string{"IS_SANDBOX": "1"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("new %s backend: %v", provider, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	session, err := backend.Execute(ctx, "workflow contract", options)
	if err != nil {
		cancel()
		t.Fatalf("execute %s backend: %v", provider, err)
	}
	for {
		select {
		case message, ok := <-session.Messages:
			if !ok {
				cancel()
				t.Fatalf("%s closed without an interaction", provider)
			}
			if message.Interaction != nil {
				return *message.Interaction, session, cancel
			}
		case <-ctx.Done():
			cancel()
			t.Fatalf("wait for %s interaction: %v", provider, ctx.Err())
		}
	}
}

func workflowContractInteractionResponse(request agent.InteractionRequest) agent.InteractionResponse {
	response := agent.InteractionResponse{ID: "workflow-contract-response", InteractionID: request.ID, ExpectedTurnID: request.TurnID}
	if request.Kind == "approval" {
		response.ChoiceID = "allow_once"
		return response
	}
	question := request.Questions[0]
	response.Answers = []agent.InteractionAnswer{{QuestionID: question.ID, OptionIDs: []string{question.Options[0].ID}}}
	return response
}

func workflowWriteFakeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake executable %s: %v", path, err)
	}
}

func workflowContractAdapterScript(provider string) string {
	switch provider {
	case "codex":
		return `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.0.0-test"; exit 0; fi
read initialize
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read initialized
read thread_start
echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-contract"}}}'
read turn_start
echo '{"jsonrpc":"2.0","id":3,"result":{}}'
echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thread-contract","turn":{"id":"turn-contract"}}}'
echo '{"jsonrpc":"2.0","id":90,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-contract","turnId":"turn-contract","itemId":"command-contract","command":"pwd"}}'
read approval
echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-contract","turn":{"id":"turn-contract","status":"completed"}}}'
`
	case "claude":
		return `#!/bin/sh
IFS= read -r prompt
echo '{"type":"system","session_id":"claude-contract"}'
echo '{"type":"control_request","request_id":"claude-contract-request","request":{"subtype":"tool_use","tool_name":"Bash","input":{"command":"pwd"}}}'
IFS= read -r response
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-contract","result":"completed"}'
`
	case "pi":
		return `#!/bin/sh
case "$*" in *"--mode rpc"*) ;; *) echo "missing Pi RPC mode" >&2; exit 61 ;; esac
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"agent_start"}\n'
printf '{"type":"turn_start"}\n'
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '{"type":"extension_ui_request","id":"pi-contract-request","method":"select","title":"Choose one","options":["One","Two"]}\n'
IFS= read -r response
printf '{"type":"agent_settled"}\n'
`
	default:
		panic("unsupported workflow contract provider: " + provider)
	}
}

func TestWorkflowContract_PreservesValidExplicitDeadline(t *testing.T) {
	now := time.Now().UTC()
	want := now.Add(5 * time.Minute)
	got, err := normalizeWorkflowInteractionDeadline(want, now)
	if err != nil {
		t.Fatalf("normalize explicit deadline: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("explicit deadline = %s, want %s", got, want)
	}
}

func TestWorkflowContract_RejectsExplicitDeadlineBeyondAdmissionBound(t *testing.T) {
	now := time.Now().UTC()
	_, err := normalizeWorkflowInteractionDeadline(now.Add(workflowInteractionDeadline+time.Second), now)
	if err == nil || !strings.Contains(err.Error(), "exceeds the maximum interaction admission") {
		t.Fatalf("normalize deadline beyond admission bound error = %v", err)
	}
}

func TestWorkflowContract_ExpirySettlesLocalAdmissionAndWakesControls(t *testing.T) {
	expired := make(chan agent.InteractionResponse, 1)
	run := &workflowRun{
		runID: "run",
		session: &agent.Session{RespondToInteraction: func(_ context.Context, response agent.InteractionResponse) (agent.InputDelivery, error) {
			expired <- response
			return agent.InputDelivery{State: "accepted"}, nil
		}},
		pending: make(map[string]agent.InteractionRequest), resolving: make(map[string]string), expiring: make(map[string]bool),
		wake: make(chan struct{}, 1),
	}
	run.presentInteraction(agent.InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "approval", ExpiresAt: time.Now().Add(25 * time.Millisecond)})
	<-run.wake // Admission's initial report signal.
	select {
	case response := <-expired:
		if !response.Cancelled || response.InteractionID != "interaction" || response.ExpectedTurnID != "turn" {
			t.Fatalf("expiry response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("interaction deadline did not trigger local denial")
	}
	select {
	case <-run.wake:
	case <-time.After(time.Second):
		t.Fatal("interaction expiry did not wake the control-state report loop")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.stateDirty {
		t.Fatal("interaction expiry did not mark controls dirty")
	}
	if len(run.pending) != 0 || len(run.interactions) != 0 {
		t.Fatalf("expiry retained daemon admission: pending=%#v reports=%#v", run.pending, run.interactions)
	}
}

func TestWorkflowContract_ExpiryWhileResolvingWakesControls(t *testing.T) {
	request := agent.InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "question"}
	run := &workflowRun{
		runID:        "run",
		session:      &agent.Session{},
		interactions: []protocol.AgentWorkflowInteractionRequest{workflowInteractionRequest(request)},
		pending:      map[string]agent.InteractionRequest{request.ID: request},
		resolving:    map[string]string{request.ID: "command"},
		expiring:     make(map[string]bool),
		wake:         make(chan struct{}, 1),
	}
	run.expireInteraction(request.ID, time.Now().Add(-time.Second))
	select {
	case <-run.wake:
	case <-time.After(time.Second):
		t.Fatal("resolving interaction expiry did not wake the control-state report loop")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.stateDirty || !run.expiring[request.ID] {
		t.Fatalf("resolving expiry state = dirty:%t expiring:%#v", run.stateDirty, run.expiring)
	}
	if len(run.interactions) != 0 {
		t.Fatalf("resolving expiry retained queued interaction report: %#v", run.interactions)
	}
	if _, exists := run.pending[request.ID]; !exists {
		t.Fatal("resolving expiry removed the provider request before its write settled")
	}
}

func TestWorkflowContract_RejectsInvalidExplicitDeadlineWithoutReporting(t *testing.T) {
	for name, deadline := range map[string]time.Time{
		"expired":            time.Now().Add(-time.Second),
		"not JSON encodable": time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			rejected := make(chan agent.InteractionResponse, 1)
			run := &workflowRun{
				daemon: &Daemon{logger: slog.New(slog.NewTextHandler(&logs, nil))},
				session: &agent.Session{RespondToInteraction: func(_ context.Context, response agent.InteractionResponse) (agent.InputDelivery, error) {
					rejected <- response
					return agent.InputDelivery{State: "accepted"}, nil
				}},
				pending: make(map[string]agent.InteractionRequest), resolving: make(map[string]string), expiring: make(map[string]bool),
				wake: make(chan struct{}, 1),
			}
			run.presentInteraction(agent.InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "approval", ExpiresAt: deadline})
			select {
			case response := <-rejected:
				if !response.Cancelled || response.InteractionID != "interaction" {
					t.Fatalf("invalid deadline response = %#v", response)
				}
			case <-time.After(time.Second):
				t.Fatal("invalid explicit deadline did not reject the provider interaction")
			}
			run.mu.Lock()
			pending, reports := len(run.pending), len(run.interactions)
			run.mu.Unlock()
			if pending != 0 || reports != 0 {
				t.Fatalf("invalid deadline entered report retry state: pending=%d reports=%d", pending, reports)
			}
			if !bytes.Contains(logs.Bytes(), []byte("invalid interaction deadline")) {
				t.Fatalf("invalid deadline was not visible in daemon logs: %s", logs.String())
			}
		})
	}
}

func TestWorkflowContract_CapturesDaemonShapesForHandlerProjection(t *testing.T) {
	importResult := workflowCompleted(struct {
		Messages []workflowHistoryMessage `json:"messages"`
		Warnings []string                 `json:"warnings"`
	}{
		Messages: workflowHistoryMessages([]agent.NativeHistoryMessage{
			{NativeID: "user", Role: "user", Content: "Question", CreatedAt: time.Unix(1, 0).UTC()},
			{NativeID: "assistant", Role: "assistant", Content: "Answer", CreatedAt: time.Unix(2, 0).UTC()},
		}),
	})
	importWire, err := json.Marshal(importResult)
	if err != nil {
		t.Fatalf("serialize daemon import result: %v", err)
	}
	var importEnvelope struct {
		Result struct {
			Messages []struct {
				Role   string          `json:"role"`
				Events json.RawMessage `json:"events"`
			} `json:"messages"`
			Warnings json.RawMessage `json:"warnings"`
		} `json:"result"`
	}
	if err := json.Unmarshal(importWire, &importEnvelope); err != nil {
		t.Fatalf("decode daemon import result: %v", err)
	}
	if len(importEnvelope.Result.Messages) != 2 {
		t.Fatalf("messages = %#v", importEnvelope.Result.Messages)
	}
	for _, message := range importEnvelope.Result.Messages {
		if string(message.Events) != "[]" {
			t.Fatalf("%s events = %s, want []", message.Role, message.Events)
		}
	}
	if string(importEnvelope.Result.Warnings) != "null" {
		t.Fatalf("warning-free import warnings = %s, want null", importEnvelope.Result.Warnings)
	}

	controlWire, err := json.Marshal(workflowDeliveryResult(agent.InputDelivery{State: "accepted"}, nil))
	if err != nil {
		t.Fatalf("serialize daemon control result: %v", err)
	}
	var controlEnvelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(controlWire, &controlEnvelope); err != nil {
		t.Fatalf("decode daemon control result: %v", err)
	}
	if string(controlEnvelope.Result) != `{"delivery":"accepted"}` {
		t.Fatalf("private daemon control result = %s", controlEnvelope.Result)
	}

	fixture, err := json.MarshalIndent(struct {
		NativeSessionImportResult json.RawMessage `json:"native_session_import_result"`
		TaskControlResult         json.RawMessage `json:"task_control_result"`
	}{
		NativeSessionImportResult: importWire,
		TaskControlResult:         controlWire,
	}, "", "  ")
	if err != nil {
		t.Fatalf("encode daemon handler fixture: %v", err)
	}
	const fixturePath = "/tmp/multica-daemon-workflow-contract-fixtures.json"
	if err := os.WriteFile(fixturePath, append(fixture, '\n'), 0o600); err != nil {
		t.Fatalf("write daemon handler fixture: %v", err)
	}
	t.Logf("wrote actual daemon workflow payloads to %s", fixturePath)
}
