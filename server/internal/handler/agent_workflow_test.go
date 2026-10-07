package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type workflowFixture struct {
	runtime, daemon, agent, session, task, run, turn string
}

func newWorkflowFixture(t *testing.T) workflowFixture {
	t.Helper()
	f := workflowFixture{daemon: "workflow-" + uuid.NewString(), run: uuid.NewString(), turn: "turn-1"}
	f.runtime = dbfx.Runtime(t, "workflow contract", testutil.Cols{
		"daemon_id": f.daemon, "runtime_mode": "local", "provider": "codex",
		"agent_workflow_capabilities": `{"native_sessions":{"list":true,"import":true},"controls":{"steer":true,"approvals":true,"questions":true}}`,
	})
	f.agent = dbfx.Agent(t, "workflow "+f.daemon, f.runtime)
	f.session = dbfx.ChatSession(t, f.agent, testutil.Cols{"runtime_id": f.runtime, "explicitly_created_at": testutil.Raw("now()")})
	f.task = dbfx.Task(t, f.agent, testutil.Cols{"runtime_id": f.runtime, "chat_session_id": f.session, "status": "running", "active_run_id": f.run, "interaction_mode": "chat"})
	// These handlers create dependents without foreign keys. Delete only this
	// fixture's runtime rows before the typed fixture builders delete parents.
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE runtime_id=$1`, f.runtime)
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id IN (SELECT id FROM chat_session WHERE runtime_id=$1)`, f.runtime)
	dbfx.Cleanup(t, `DELETE FROM agent_workflow_request WHERE runtime_id=$1`, f.runtime)
	dbfx.Cleanup(t, `DELETE FROM task_interaction WHERE runtime_id=$1`, f.runtime)
	testutil.Call(t, testHandler.ReportTaskControls, f.daemonRequest("controls", map[string]any{
		"run_id": f.run, "turn_id": f.turn, "active": true, "can_steer": true, "can_approve": true, "can_answer": true,
	})).Want(http.StatusOK)
	return f
}

func workflowUserRequest(t *testing.T, method string, body any, params ...string) *http.Request {
	t.Helper()
	return testutil.WithURLParams(chatPendingCtxAs(t, newRequest(method, "/agent-workflow-contract", body), testUserID), params...)
}

func (f workflowFixture) daemonRequest(endpoint string, body any) *http.Request {
	return testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+f.task+"/"+endpoint, body, testWorkspaceID, f.daemon), "taskId", f.task)
}

func (f workflowFixture) reportRequest(id string, result any) *http.Request {
	return testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+f.runtime+"/agent-workflow-requests/"+id+"/result", map[string]any{"status": "completed", "result": result}, testWorkspaceID, f.daemon), "runtimeId", f.runtime, "requestId", id)
}

func (f workflowFixture) dispatchWorkflow(t *testing.T) {
	t.Helper()
	_, err := testHandler.claimAgentWorkflowRequests(context.Background(), parseUUID(f.runtime), protocol.AgentWorkflowCapabilities{
		NativeSessions: protocol.AgentWorkflowNativeSessionCapabilities{List: true, Import: true},
		Controls:       protocol.AgentWorkflowControlCapabilities{Steer: true, Approvals: true, Questions: true},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f workflowFixture) listSource(t *testing.T, revision string) string {
	t.Helper()
	id := uuid.NewString()
	testutil.Call(t, testHandler.InitiateNativeSessionList, workflowUserRequest(t, http.MethodPost, map[string]any{"request_id": id, "limit": 20}, "runtimeId", f.runtime)).Want(http.StatusAccepted)
	f.dispatchWorkflow(t)
	result := testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(id, map[string]any{"sessions": []any{map[string]any{
		"native_id": "original-source", "revision": revision, "handle": "/fake/private/source", "title": "Imported history", "cwd": "/fake/project",
	}}})).Want(http.StatusOK).Map()
	page := result["result"].(map[string]any)
	if cursor, present := page["next_cursor"]; !present || cursor != nil {
		t.Fatalf("completed list must contain next_cursor:null, got %v", page)
	}
	if page["truncated"] != false {
		t.Fatalf("completed list must contain truncated:false, got %v", page)
	}
	sessions, ok := page["sessions"].([]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("completed list must contain one session, got %v", page)
	}
	public := sessions[0].(map[string]any)
	if _, exposed := public["handle"]; exposed {
		t.Fatal("private source handle exposed")
	}
	return public["session_ref"].(string)
}

func (f workflowFixture) importBody(ref, revision string) map[string]any {
	return map[string]any{"request_id": uuid.NewString(), "session_ref": ref, "revision": revision, "agent_id": f.agent}
}

func (f workflowFixture) beginImport(t *testing.T, ref, revision string) (string, string, map[string]any) {
	t.Helper()
	body := f.importBody(ref, revision)
	testutil.Call(t, testHandler.InitiateNativeSessionImport, workflowUserRequest(t, http.MethodPost, body, "runtimeId", f.runtime)).Want(http.StatusAccepted)
	f.dispatchWorkflow(t)
	var commandJSON []byte
	id := body["request_id"].(string)
	dbfx.QueryRow(t, `SELECT request FROM agent_workflow_request WHERE id=$1`, id).Scan(&commandJSON)
	var command map[string]any
	if err := json.Unmarshal(commandJSON, &command); err != nil {
		t.Fatal(err)
	}
	if command["agent_id"] != f.agent || command["native_id"] != "original-source" {
		t.Fatalf("wrong private import ownership: %v", command)
	}
	if _, err := uuid.Parse(command["chat_session_id"].(string)); err != nil {
		t.Fatalf("missing preallocated chat: %v", command)
	}
	return id, command["chat_session_id"].(string), map[string]any{
		"native_id": "original-source", "owned_native_id": "owned-clone",
		"provider": "codex", "resume_session_id": "owned-clone", "work_dir": "/fake/project",
		"messages": []any{map[string]any{"native_id": "m1", "role": "user", "content": "Question", "created_at": "2026-01-01T00:00:00Z"}, map[string]any{"native_id": "m2", "role": "assistant", "content": "Answer", "created_at": "2026-01-01T00:00:01Z", "events": []any{map[string]any{"seq": 1, "type": "text", "content": "Answer"}}}},
		"warnings": []string{},
	}
}

func TestAgentWorkflowControlsAndEmptyInteractionsContract(t *testing.T) {
	f := newWorkflowFixture(t)
	controls := testutil.Call(t, testHandler.GetChatControls, workflowUserRequest(t, http.MethodGet, nil, "sessionId", f.session)).Want(http.StatusOK).Map()
	for key, want := range map[string]any{"active": true, "runtime_id": f.runtime, "chat_session_id": f.session, "task_id": f.task, "run_id": f.run, "turn_id": f.turn} {
		if controls[key] != want {
			t.Errorf("controls.%s=%v, want %v", key, controls[key], want)
		}
	}
	interactions := testutil.Call(t, testHandler.ListChatInteractions, workflowUserRequest(t, http.MethodGet, nil, "sessionId", f.session)).Want(http.StatusOK).Map()
	items, ok := interactions["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("want {items:[]}, got %v", interactions)
	}
}

func TestAgentWorkflowNativeImportRollbackReplayAndSourceDedup(t *testing.T) {
	f := newWorkflowFixture(t)
	ref := f.listSource(t, "rev-1")
	id, sessionID, report := f.beginImport(t, ref, "rev-1")
	for _, field := range []string{"native_id", "provider"} {
		original := report[field]
		report[field] = uuid.NewString()
		testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(id, report)).Want(http.StatusConflict)
		report[field] = original
	}
	wrongDaemon := testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/import-result", map[string]any{"status": "completed", "result": report}, testWorkspaceID, "unrelated-daemon"), "runtimeId", f.runtime, "requestId", id)
	testutil.Call(t, testHandler.ReportAgentWorkflowResult, wrongDaemon).Want(http.StatusNotFound)
	// Warnings are checked after inserts: this specifically exercises rollback,
	// rather than merely rejecting invalid JSON before a transaction starts.
	report["warnings"] = []string{" "}
	testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(id, report)).Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id=$1`, sessionID); n != 0 {
		t.Fatalf("partial chat published: %d", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1`, sessionID); n != 0 {
		t.Fatalf("partial messages published: %d", n)
	}
	report["warnings"] = []string{}
	for range 2 {
		testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(id, report)).Want(http.StatusOK)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1`, sessionID); n != 2 {
		t.Fatalf("report replay duplicated messages: %d", n)
	}
	var source, resume string
	dbfx.QueryRow(t, `SELECT native_import_id, session_id FROM chat_session WHERE id=$1`, sessionID).Scan(&source, &resume)
	if source != "original-source" || resume != "owned-clone" {
		t.Fatalf("source/resume=%s/%s", source, resume)
	}
	newRef := f.listSource(t, "rev-2")
	duplicate := testutil.Call(t, testHandler.InitiateNativeSessionImport, workflowUserRequest(t, http.MethodPost, f.importBody(newRef, "rev-2"), "runtimeId", f.runtime)).Want(http.StatusOK).Map()["result"].(map[string]any)
	if duplicate["chat_session_id"] != sessionID || duplicate["already_imported"] != true {
		t.Fatalf("revision changed source identity: %v", duplicate)
	}
	other := newWorkflowFixture(t)
	otherID, otherSessionID, otherReport := other.beginImport(t, other.listSource(t, "rev-2"), "rev-2")
	testutil.Call(t, testHandler.ReportAgentWorkflowResult, other.reportRequest(otherID, otherReport)).Want(http.StatusOK)
	if otherSessionID == sessionID {
		t.Fatal("dedup crossed runtime boundary")
	}
}

func TestAgentWorkflowNativeReferenceOwnership(t *testing.T) {
	f, other := newWorkflowFixture(t), newWorkflowFixture(t)
	ref := f.listSource(t, "rev-1")
	testutil.Call(t, testHandler.InitiateNativeSessionImport, workflowUserRequest(t, http.MethodPost, other.importBody(ref, "rev-1"), "runtimeId", other.runtime)).Want(http.StatusGone)
	user := dbfx.User(t, "Other workflow user", uuid.NewString()+"@example.invalid")
	dbfx.Member(t, testWorkspaceID, user, "member")
	req := chatPendingCtxAs(t, newRequestAsUser(user, http.MethodPost, "/native-import", f.importBody(ref, "rev-1")), user)
	testutil.Call(t, testHandler.InitiateNativeSessionImport, testutil.WithURLParams(req, "runtimeId", f.runtime)).Want(http.StatusForbidden)
	// Even after ownership transfer, a list reference stays scoped to the user
	// who obtained it, rather than becoming a bearer credential.
	dbfx.Exec(t, `UPDATE agent_runtime SET owner_id=$1 WHERE id=$2`, user, f.runtime)
	req = chatPendingCtxAs(t, newRequestAsUser(user, http.MethodPost, "/native-import", f.importBody(ref, "rev-1")), user)
	testutil.Call(t, testHandler.InitiateNativeSessionImport, testutil.WithURLParams(req, "runtimeId", f.runtime)).Want(http.StatusGone)
}

func (f workflowFixture) question(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	testutil.Call(t, testHandler.ReportTaskInteraction, f.daemonRequest("interactions", map[string]any{"run_id": f.run, "interaction": map[string]any{
		"id": id, "turn_id": f.turn, "kind": "question", "title": "Choose scope", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"questions": []any{map[string]any{"id": "scope", "prompt": "Scope?", "multiple": false, "allow_text": false, "options": []any{map[string]any{"id": "unit", "label": "Unit"}, map[string]any{"id": "all", "label": "All"}}}},
	}})).Want(http.StatusOK)
	return id
}

func (f workflowFixture) answerBody(response any) map[string]any {
	return map[string]any{"request_id": uuid.NewString(), "task_id": f.task, "run_id": f.run, "turn_id": f.turn, "response": response}
}

func workflowAnswer(options ...string) map[string]any {
	return map[string]any{"answers": []any{map[string]any{"question_id": "scope", "option_ids": options}}}
}

func TestAgentWorkflowQuestionValidationAndConcurrentResolution(t *testing.T) {
	f := newWorkflowFixture(t)
	id := f.question(t)
	for name, answer := range map[string]any{
		"unknown option": workflowAnswer("invented"), "cardinality": workflowAnswer("unit", "all"), "duplicate option": workflowAnswer("unit", "unit"),
		"missing answer": map[string]any{"answers": []any{}}, "wrong branch": map[string]any{"choice_id": "unit"},
		"unoffered text": map[string]any{"answers": []any{map[string]any{"question_id": "scope", "text": "unit"}}},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.RespondChatInteraction, workflowUserRequest(t, http.MethodPost, f.answerBody(answer), "sessionId", f.session, "interactionId", id)).Want(http.StatusBadRequest)
		})
	}
	bodies := []map[string]any{f.answerBody(workflowAnswer("unit")), f.answerBody(workflowAnswer("all"))}
	requests := []*http.Request{workflowUserRequest(t, http.MethodPost, bodies[0], "sessionId", f.session, "interactionId", id), workflowUserRequest(t, http.MethodPost, bodies[1], "sessionId", f.session, "interactionId", id)}
	responses := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; testHandler.RespondChatInteraction(responses[i], requests[i]) }(i)
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, response := range responses {
		if response.Code == http.StatusAccepted {
			if winner != -1 {
				t.Fatal("both CAS responses won")
			}
			winner = i
		} else if response.Code != http.StatusConflict {
			t.Fatalf("unexpected CAS result %d: %s", response.Code, response.Body)
		}
	}
	if winner == -1 {
		t.Fatal("neither CAS response won")
	}
	requestID := bodies[winner]["request_id"]
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_workflow_request WHERE task_id=$1 AND kind='interaction_response'`, f.task); n != 1 {
		t.Fatalf("want exactly one response command, got %d", n)
	}
	f.dispatchWorkflow(t)
	testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(requestID.(string), map[string]any{"delivery": "accepted"})).Want(http.StatusOK)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM task_interaction WHERE id=$1`, id).Scan(&status)
	if status != "resolved" {
		t.Errorf("successful daemon response left interaction %s", status)
	}
	replay := testutil.Call(t, testHandler.RespondChatInteraction, workflowUserRequest(t, http.MethodPost, bodies[winner], "sessionId", f.session, "interactionId", id)).Want(http.StatusOK).Map()
	if replay["id"] != requestID || replay["status"] != "completed" {
		t.Fatalf("resolved request replay changed result: %v", replay)
	}
}

func TestAgentWorkflowStaleTurnsAndDaemonOwnership(t *testing.T) {
	f := newWorkflowFixture(t)
	for _, field := range []string{"task_id", "run_id", "turn_id"} {
		t.Run(field, func(t *testing.T) {
			body := map[string]any{"request_id": uuid.NewString(), "task_id": f.task, "run_id": f.run, "turn_id": f.turn, "content": "Steer exact turn"}
			body[field] = uuid.NewString()
			testutil.Call(t, testHandler.InitiateChatSteer, workflowUserRequest(t, http.MethodPost, body, "sessionId", f.session)).Want(http.StatusConflict)
		})
	}
	state := map[string]any{"run_id": uuid.NewString(), "turn_id": f.turn, "active": true}
	testutil.Call(t, testHandler.ReportTaskControls, f.daemonRequest("controls", state)).Want(http.StatusConflict)
	state["run_id"] = f.run
	wrongDaemon := testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/controls", state, testWorkspaceID, "unrelated-daemon"), "taskId", f.task)
	testutil.Call(t, testHandler.ReportTaskControls, wrongDaemon).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.ReportTaskControls, workflowUserRequest(t, http.MethodPost, state, "taskId", f.task)).Want(http.StatusForbidden)
	id := f.question(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET active_run_id=$1 WHERE id=$2`, uuid.NewString(), f.task)
	testutil.Call(t, testHandler.RespondChatInteraction, workflowUserRequest(t, http.MethodPost, f.answerBody(workflowAnswer("unit")), "sessionId", f.session, "interactionId", id)).Want(http.StatusConflict)
}

func TestAgentWorkflowApprovalUsesOnlyOfferedChoice(t *testing.T) {
	f := newWorkflowFixture(t)
	id := uuid.NewString()
	testutil.Call(t, testHandler.ReportTaskInteraction, f.daemonRequest("interactions", map[string]any{"run_id": f.run, "interaction": map[string]any{
		"id": id, "turn_id": f.turn, "kind": "approval", "title": "Run tests", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"choices": []any{map[string]any{"id": "allow_once", "label": "Allow once"}, map[string]any{"id": "deny", "label": "Deny"}},
	}})).Want(http.StatusOK)
	for _, response := range []any{map[string]any{"choice_id": "allow_always"}, map[string]any{"choice_id": "allow_once", "cancelled": true}, workflowAnswer("unit")} {
		testutil.Call(t, testHandler.RespondChatInteraction, workflowUserRequest(t, http.MethodPost, f.answerBody(response), "sessionId", f.session, "interactionId", id)).Want(http.StatusBadRequest)
	}
	body := f.answerBody(map[string]any{"choice_id": "allow_once"})
	testutil.Call(t, testHandler.RespondChatInteraction, workflowUserRequest(t, http.MethodPost, body, "sessionId", f.session, "interactionId", id)).Want(http.StatusAccepted)
	var raw []byte
	dbfx.QueryRow(t, `SELECT request FROM agent_workflow_request WHERE id=$1`, body["request_id"]).Scan(&raw)
	var command map[string]any
	if err := json.Unmarshal(raw, &command); err != nil {
		t.Fatal(err)
	}
	if command["interaction_id"] != id || command["response"].(map[string]any)["choice_id"] != "allow_once" {
		t.Fatalf("approval mapping changed: %s", raw)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1`, f.session); n != 1 {
		t.Fatalf("approval queued an extra prompt: %d", n)
	}
}
