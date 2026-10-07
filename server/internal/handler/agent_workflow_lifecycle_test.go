package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// workflowLifecycleHeartbeat is the only dispatch boundary these tests use.
// A claimed row is already a native command emission, even though the test
// daemon never invokes a provider.
func workflowLifecycleHeartbeat(t *testing.T, f workflowFixture) map[string]any {
	t.Helper()
	return testutil.Call(t, testHandler.DaemonHeartbeat, newDaemonTokenRequest(http.MethodPost, "/api/daemon/heartbeat", map[string]any{
		"runtime_id": f.runtime,
		"agent_workflow_capabilities": map[string]any{
			"native_sessions": map[string]any{"list": true, "import": true},
			"controls":        map[string]any{"steer": true, "approvals": true, "questions": true},
		},
	}, testWorkspaceID, f.daemon)).Want(http.StatusOK).Map()
}

func workflowLifecycleRequestStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_workflow_request WHERE id=$1`, id).Scan(&status)
	return status
}

func workflowLifecycleInteractionStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	dbfx.QueryRow(t, `SELECT status FROM task_interaction WHERE id=$1`, id).Scan(&status)
	return status
}

func workflowLifecycleNoEmission(t *testing.T, heartbeat map[string]any) {
	t.Helper()
	if emitted, ok := heartbeat["pending_agent_workflow"]; ok {
		t.Fatalf("invalid workflow command reached heartbeat: %v", emitted)
	}
}

func workflowLifecycleUserRequestAs(t *testing.T, userID, method string, body any, params ...string) *http.Request {
	t.Helper()
	req := newRequest(method, "/agent-workflow-lifecycle", body)
	req.Header.Set("X-User-ID", userID)
	return testutil.WithURLParams(chatPendingCtxAs(t, req, userID), params...)
}

func (f workflowFixture) lifecycleApproval(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	testutil.Call(t, testHandler.ReportTaskInteraction, f.daemonRequest("interactions", map[string]any{"run_id": f.run, "interaction": map[string]any{
		"id": id, "turn_id": f.turn, "kind": "approval", "title": "Run lifecycle action", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"choices": []any{map[string]any{"id": "allow_once", "label": "Allow once"}, map[string]any{"id": "deny", "label": "Deny"}},
	}})).Want(http.StatusOK)
	return id
}

func workflowLifecyclePendingApprovalResponse(t *testing.T) (workflowFixture, string, string) {
	t.Helper()
	f := newWorkflowFixture(t)
	interactionID := f.lifecycleApproval(t)
	body := f.answerBody(map[string]any{"choice_id": "allow_once"})
	testutil.Call(t, testHandler.RespondChatInteraction, workflowUserRequest(t, http.MethodPost, body, "sessionId", f.session, "interactionId", interactionID)).Want(http.StatusAccepted)
	return f, interactionID, body["request_id"].(string)
}

func TestAgentWorkflowLifecycle_RetiresDeadInteractionsAndAdmitsNextRun(t *testing.T) {
	for name, retire := range map[string]func(t *testing.T, f workflowFixture){
		"task terminal": func(t *testing.T, f workflowFixture) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, f.task)
			testutil.Call(t, testHandler.ListChatInteractions, workflowUserRequest(t, http.MethodGet, nil, "sessionId", f.session)).Want(http.StatusOK)
		},
		"run replaced": func(t *testing.T, f workflowFixture) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET active_run_id=$1 WHERE id=$2`, uuid.NewString(), f.task)
			testutil.Call(t, testHandler.ListChatInteractions, workflowUserRequest(t, http.MethodGet, nil, "sessionId", f.session)).Want(http.StatusOK)
		},
		"inactive controls": func(t *testing.T, f workflowFixture) {
			testutil.Call(t, testHandler.ReportTaskControls, f.daemonRequest("controls", map[string]any{
				"run_id": f.run, "active": false,
			})).Want(http.StatusOK)
		},
		"expired interaction": func(t *testing.T, f workflowFixture) {
			dbfx.Exec(t, `UPDATE task_interaction SET expires_at=now() - interval '1 second' WHERE task_id=$1`, f.task)
			testutil.Call(t, testHandler.ReportTaskControls, f.daemonRequest("controls", map[string]any{
				"run_id": f.run, "turn_id": f.turn, "active": true, "can_steer": true, "can_approve": true, "can_answer": true,
			})).Want(http.StatusOK)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkflowFixture(t)
			interactionID := f.question(t)
			retire(t, f)

			if status := workflowLifecycleInteractionStatus(t, interactionID); status == "pending" || status == "resolving" || status == "unknown" {
				t.Fatalf("%s left interaction %s unsettled", name, status)
			}
			items := testutil.Call(t, testHandler.ListChatInteractions, workflowUserRequest(t, http.MethodGet, nil, "sessionId", f.session)).Want(http.StatusOK).Map()["items"].([]any)
			if len(items) != 0 {
				t.Fatalf("%s left an interaction visible to the next run: %v", name, items)
			}
		})
	}

	// The replacement case is also the user-visible recovery boundary: a fresh
	// control state must accept steering after a former run asked a question.
	f := newWorkflowFixture(t)
	oldInteraction := f.question(t)
	newRun := uuid.NewString()
	dbfx.Exec(t, `UPDATE agent_task_queue SET active_run_id=$1 WHERE id=$2`, newRun, f.task)
	testutil.Call(t, testHandler.ReportTaskControls, f.daemonRequest("controls", map[string]any{
		"run_id": newRun, "turn_id": "turn-2", "active": true, "can_steer": true,
	})).Want(http.StatusOK)
	steerID := uuid.NewString()
	testutil.Call(t, testHandler.InitiateChatSteer, workflowUserRequest(t, http.MethodPost, map[string]any{
		"request_id": steerID, "task_id": f.task, "run_id": newRun, "turn_id": "turn-2", "content": "continue with the new run",
	}, "sessionId", f.session)).Want(http.StatusAccepted)
	if status := workflowLifecycleInteractionStatus(t, oldInteraction); status == "pending" || status == "resolving" || status == "unknown" {
		t.Fatalf("replacement left old interaction %s", status)
	}
}

func TestAgentWorkflowLifecycle_DispatchedLateInteractionAckReconcilesWithoutRedispatch(t *testing.T) {
	f, interactionID, requestID := workflowLifecyclePendingApprovalResponse(t)
	heartbeat := workflowLifecycleHeartbeat(t, f)
	if emitted, ok := heartbeat["pending_agent_workflow"].([]any); !ok || len(emitted) != 1 {
		t.Fatalf("expected exactly one dispatched interaction command, got %v", heartbeat)
	}

	testutil.Call(t, testHandler.ReportTaskControls, f.daemonRequest("controls", map[string]any{
		"run_id": f.run, "active": false,
	})).Want(http.StatusOK)
	if status := workflowLifecycleRequestStatus(t, requestID); status != "unknown" {
		t.Fatalf("dispatched command status = %q, want unknown for late acknowledgement", status)
	}
	if status := workflowLifecycleInteractionStatus(t, interactionID); status != "cancelled" {
		t.Fatalf("resolving interaction status = %q, want durable cancellation after retired run", status)
	}

	testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(requestID, map[string]any{"delivery": "accepted"})).Want(http.StatusOK)
	if status := workflowLifecycleInteractionStatus(t, interactionID); status != "resolved" {
		t.Fatalf("accepted late acknowledgement left interaction %q", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_workflow_request WHERE id=$1`, requestID); n != 1 {
		t.Fatalf("late acknowledgement created %d commands, want original only", n)
	}
	workflowLifecycleNoEmission(t, workflowLifecycleHeartbeat(t, f))
}

func TestAgentWorkflowLifecycle_DispatchAdmissionRejectsRevokedOrStaleCommands(t *testing.T) {
	tests := map[string]func(t *testing.T, f workflowFixture, interactionID, requestID string){
		"invocation revoked": func(t *testing.T, f workflowFixture, _ string, _ string) {
			ownerID := dbfx.User(t, "workflow other owner", "workflow-other-owner-"+uuid.NewString()+"@example.test")
			dbfx.Exec(t, `UPDATE agent SET owner_id=$1, permission_mode='public_to' WHERE id=$2`, ownerID, f.agent)
			dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2)`, f.agent, testWorkspaceID)
			// The command was accepted while the workspace grant existed; removing
			// it before heartbeat must stop delivery, not merely later publication.
			dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, f.agent)
		},
		"agent archived": func(t *testing.T, f workflowFixture, _ string, _ string) {
			dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, f.agent)
		},
		"runtime rebound": func(t *testing.T, f workflowFixture, _ string, _ string) {
			otherRuntime := dbfx.Runtime(t, "workflow rebound runtime", testutil.Cols{"daemon_id": "workflow-rebound-" + uuid.NewString(), "runtime_mode": "local", "provider": "codex"})
			dbfx.Exec(t, `UPDATE agent SET runtime_id=$1 WHERE id=$2`, otherRuntime, f.agent)
		},
		"task terminal": func(t *testing.T, f workflowFixture, _ string, _ string) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, f.task)
		},
		"run replaced": func(t *testing.T, f workflowFixture, _ string, _ string) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET active_run_id=$1 WHERE id=$2`, uuid.NewString(), f.task)
		},
		"turn replaced": func(t *testing.T, f workflowFixture, _ string, _ string) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET control_state=jsonb_build_object('run_id', $1::text, 'turn_id', 'new-turn', 'active', true, 'can_approve', true) WHERE id=$2`, f.run, f.task)
		},
		"response request replaced": func(t *testing.T, _ workflowFixture, interactionID, _ string) {
			dbfx.Exec(t, `UPDATE task_interaction SET response_request_id=$1 WHERE id=$2`, uuid.NewString(), interactionID)
		},
	}

	for name, invalidate := range tests {
		t.Run(name, func(t *testing.T) {
			f, interactionID, requestID := workflowLifecyclePendingApprovalResponse(t)
			invalidate(t, f, interactionID, requestID)
			workflowLifecycleNoEmission(t, workflowLifecycleHeartbeat(t, f))
			if status := workflowLifecycleRequestStatus(t, requestID); status != "failed" {
				t.Fatalf("invalid command status = %q, want failed", status)
			}
			workflowLifecycleNoEmission(t, workflowLifecycleHeartbeat(t, f))
			if status := workflowLifecycleRequestStatus(t, requestID); status != "failed" {
				t.Fatalf("second heartbeat changed failed command to %q", status)
			}
		})
	}
}

func seedWorkflowLifecycleRequest(t *testing.T, f workflowFixture, kind, status string) string {
	t.Helper()
	return dbfx.Insert(t, "agent_workflow_request", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": testWorkspaceID, "requester_id": testUserID, "runtime_id": f.runtime, "chat_session_id": f.session,
		"task_id": f.task, "run_id": f.run, "turn_id": f.turn, "kind": kind, "status": status,
		"request_hash": uuid.NewString(), "request": testutil.Raw(`'{}'::jsonb`), "expires_at": testutil.Raw("now() + interval '1 hour'"),
	})
}

func seedWorkflowLifecycleInteraction(t *testing.T, f workflowFixture, status string) string {
	t.Helper()
	return dbfx.Insert(t, "task_interaction", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": testWorkspaceID, "runtime_id": f.runtime, "chat_session_id": f.session, "task_id": f.task, "run_id": f.run,
		"turn_id": f.turn, "kind": "approval", "status": status, "request": testutil.Raw(`'{}'::jsonb`), "expires_at": testutil.Raw("now() + interval '1 hour'"),
	})
}

func TestAgentWorkflowLifecycle_TaskServiceTerminalTransitionsRetireInteractions(t *testing.T) {
	tests := map[string]func(t *testing.T, f workflowFixture){
		"cancel": func(t *testing.T, f workflowFixture) {
			if _, err := testHandler.TaskService.CancelTask(context.Background(), parseUUID(f.task)); err != nil {
				t.Fatalf("CancelTask: %v", err)
			}
		},
		"complete": func(t *testing.T, f workflowFixture) {
			if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(f.task), []byte(`{"ok":true}`), "", "", "", false, "", ""); err != nil {
				t.Fatalf("CompleteTask: %v", err)
			}
		},
		"recovery": func(t *testing.T, f workflowFixture) {
			rows, err := testHandler.TaskService.RecoverOrphanedTasksForRuntime(context.Background(), parseUUID(f.runtime))
			if err != nil {
				t.Fatalf("RecoverOrphanedTasksForRuntime: %v", err)
			}
			if len(rows) != 1 || uuidToString(rows[0].ID) != f.task {
				t.Fatalf("recovered tasks = %+v, want %s", rows, f.task)
			}
		},
	}
	for name, terminalize := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWorkflowFixture(t)
			interactionID := f.question(t)
			terminalize(t, f)
			if status := workflowLifecycleInteractionStatus(t, interactionID); status != "cancelled" {
				t.Fatalf("%s left interaction %q, want cancelled", name, status)
			}
			items := testutil.Call(t, testHandler.ListChatInteractions, workflowUserRequest(t, http.MethodGet, nil, "sessionId", f.session)).Want(http.StatusOK).Map()["items"].([]any)
			if len(items) != 0 {
				t.Fatalf("%s terminal transition left interactions %v", name, items)
			}
		})
	}
}

func TestAgentWorkflowLifecycle_SupplementSettlementFailureRollsBackWorkflowRetirement(t *testing.T) {
	f := newWorkflowFixture(t)
	interactionID := f.question(t)
	// The supplement update must still run when this chat has no supplement
	// receipts. Reject it after workflow retirement to prove the shared transaction.
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE FUNCTION reject_workflow_terminal_supplement_settlement() RETURNS trigger AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM agent_task_queue WHERE id=TG_ARGV[0]::uuid AND status='completed') THEN
				RAISE EXCEPTION 'supplement settlement rejected';
			END IF;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_workflow_terminal_supplement_settlement BEFORE UPDATE ON task_supplement
		FOR EACH STATEMENT EXECUTE FUNCTION reject_workflow_terminal_supplement_settlement('%s')
	`, f.task))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_workflow_terminal_supplement_settlement ON task_supplement`)
		_, _ = testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS reject_workflow_terminal_supplement_settlement()`)
	})
	if _, err := testHandler.TaskService.CompleteTask(t.Context(), parseUUID(f.task), []byte(`{"ok":true}`), "", "", "", false, "", ""); err == nil {
		t.Fatal("completion succeeded despite rejected supplement settlement")
	}
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "running" || workflowLifecycleInteractionStatus(t, interactionID) != "pending" {
		t.Fatalf("failed settlement partially committed terminal state or workflow retirement: task=%s interaction=%s", task.Status, workflowLifecycleInteractionStatus(t, interactionID))
	}
}

func TestAgentWorkflowLifecycle_DeleteChatPrunesWorkflowDependents(t *testing.T) {
	f := newWorkflowFixture(t)
	for _, status := range []string{"pending", "resolved"} {
		seedWorkflowLifecycleInteraction(t, f, status)
	}
	for _, kind := range []string{"steer", "interaction_response", "native_session_import"} {
		seedWorkflowLifecycleRequest(t, f, kind, "pending")
	}

	testutil.Call(t, testHandler.DeleteChatSession, workflowUserRequest(t, http.MethodDelete, nil, "sessionId", f.session)).Want(http.StatusNoContent)
	for table := range map[string]struct{}{"task_interaction": {}, "agent_workflow_request": {}} {
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE chat_session_id=$1`, f.session); n != 0 {
			t.Fatalf("deleted chat retained %d %s rows", n, table)
		}
	}
}

// The deleter enters the session lock queue first. A late daemon report must
// wait behind it, observe the deletion, and leave no no-FK interaction row.
func TestAgentWorkflowLifecycle_DeleteChatSerializesLateInteractionReport(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkflowFixture(t)
	ctx := context.Background()
	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lifecycle lock connection: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lifecycle lock transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM chat_session WHERE id=$1 FOR UPDATE`, f.session); err != nil {
		t.Fatalf("lock chat session: %v", err)
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.DeleteChatSession(w, workflowUserRequest(t, http.MethodDelete, nil, "sessionId", f.session))
		deleteDone <- w
	}()
	workflowLifecycleWaitForSessionLock(t, ctx, conn.Conn().PgConn().PID())

	reportDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.ReportTaskInteraction(w, f.daemonRequest("interactions", map[string]any{"run_id": f.run, "interaction": map[string]any{
			"id": uuid.NewString(), "turn_id": f.turn, "kind": "approval", "title": "late approval", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"choices": []any{map[string]any{"id": "allow_once", "label": "Allow once"}},
		}}))
		reportDone <- w
	}()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release lifecycle lock: %v", err)
	}

	deleteResponse := <-deleteDone
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete chat = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	reportResponse := <-reportDone
	if reportResponse.Code != http.StatusConflict && reportResponse.Code != http.StatusNotFound {
		t.Fatalf("late interaction report = %d: %s", reportResponse.Code, reportResponse.Body.String())
	}
	for table := range map[string]struct{}{"task_interaction": {}, "agent_workflow_request": {}} {
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE chat_session_id=$1`, f.session); n != 0 {
			t.Fatalf("late report recreated %d %s rows after deletion", n, table)
		}
	}
}

// A daemon acknowledgement has the same parent-session fence as an interaction
// report. Once deletion wins the session lock, it cannot publish the accepted
// steer message after the chat is gone.
func TestAgentWorkflowLifecycle_DeleteChatSerializesLateAcceptedSteer(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkflowFixture(t)
	requestID := uuid.NewString()
	testutil.Call(t, testHandler.InitiateChatSteer, workflowUserRequest(t, http.MethodPost, map[string]any{
		"request_id": requestID, "task_id": f.task, "run_id": f.run, "turn_id": f.turn, "content": "late accepted steer",
	}, "sessionId", f.session)).Want(http.StatusAccepted)
	workflowLifecycleHeartbeat(t, f)

	ctx := context.Background()
	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lifecycle lock connection: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lifecycle lock transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM chat_session WHERE id=$1 FOR UPDATE`, f.session); err != nil {
		t.Fatalf("lock chat session: %v", err)
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.DeleteChatSession(w, workflowUserRequest(t, http.MethodDelete, nil, "sessionId", f.session))
		deleteDone <- w
	}()
	workflowLifecycleWaitForSessionLock(t, ctx, conn.Conn().PgConn().PID())

	resultDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.ReportAgentWorkflowResult(w, f.reportRequest(requestID, map[string]any{"delivery": "accepted"}))
		resultDone <- w
	}()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release lifecycle lock: %v", err)
	}
	if w := <-deleteDone; w.Code != http.StatusNoContent {
		t.Fatalf("delete chat = %d: %s", w.Code, w.Body.String())
	}
	if w := <-resultDone; w.Code != http.StatusConflict && w.Code != http.StatusNotFound {
		t.Fatalf("late accepted steer = %d: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1`, f.session); n != 0 {
		t.Fatalf("late accepted steer published %d messages", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_workflow_request WHERE id=$1`, requestID); n != 0 {
		t.Fatalf("late accepted steer retained %d workflow rows", n)
	}
}

func TestAgentWorkflowLifecycle_DeleteSystemChatPrunesImportReservationAndAgent(t *testing.T) {
	f := newWorkflowFixture(t)
	dbfx.Exec(t, `UPDATE agent SET kind='system', system_key='agent_builder:lifecycle-cleanup' WHERE id=$1`, f.agent)
	reservationID := dbfx.Insert(t, "agent_workflow_request", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": testWorkspaceID, "requester_id": testUserID, "runtime_id": f.runtime,
		"kind": "native_session_import", "status": "pending", "request_hash": uuid.NewString(),
		"request":    testutil.Raw(`jsonb_build_object('agent_id', '` + f.agent + `')`),
		"expires_at": testutil.Raw("now() + interval '1 hour'"), "native_source_id": "system-agent-reservation",
	})
	testutil.Call(t, testHandler.DeleteChatSession, workflowUserRequest(t, http.MethodDelete, nil, "sessionId", f.session)).Want(http.StatusNoContent)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_workflow_request WHERE id=$1`, reservationID); n != 0 {
		t.Fatalf("system chat retained %d import reservations", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE id=$1`, f.agent); n != 0 {
		t.Fatalf("system chat retained %d owned agents", n)
	}
}

func workflowLifecycleWaitForSessionLock(t *testing.T, ctx context.Context, holderPID uint32) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		dbfx.QueryRow(t, `
			SELECT count(*)
			FROM pg_locks waiting
			JOIN pg_locks held ON held.locktype = waiting.locktype
				AND held.database IS NOT DISTINCT FROM waiting.database
				AND held.relation IS NOT DISTINCT FROM waiting.relation
				AND held.page IS NOT DISTINCT FROM waiting.page
				AND held.tuple IS NOT DISTINCT FROM waiting.tuple
				AND held.transactionid IS NOT DISTINCT FROM waiting.transactionid
				AND held.classid IS NOT DISTINCT FROM waiting.classid
				AND held.objid IS NOT DISTINCT FROM waiting.objid
				AND held.objsubid IS NOT DISTINCT FROM waiting.objsubid
			WHERE NOT waiting.granted AND held.granted AND held.pid=$1`, int32(holderPID)).Scan(&waiting)
		if waiting > 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("chat deletion did not wait for the lifecycle session lock")
		case <-ctx.Done():
			t.Fatalf("waiting for lifecycle session lock: %v", ctx.Err())
		case <-tick.C:
		}
	}
}

func TestAgentWorkflowLifecycle_ActualDaemonImportWireRepresentations(t *testing.T) {
	t.Run("empty user and assistant timelines with nil warnings are public success", func(t *testing.T) {
		f := newWorkflowFixture(t)
		ref := f.listSource(t, "wire-revision")
		requestID, _, report := f.beginImport(t, ref, "wire-revision")
		// These are the daemon's concrete JSON encodings. In particular, events
		// is an allocated empty array, while warnings is nil and serializes null.
		report["messages"] = []any{
			map[string]any{"native_id": "user-wire", "role": "user", "content": "Question", "created_at": "2026-01-01T00:00:00Z", "events": []any{}},
			map[string]any{"native_id": "assistant-empty-wire", "role": "assistant", "content": "Answer", "created_at": "2026-01-01T00:00:01Z", "events": []any{}},
			map[string]any{"native_id": "assistant-event-wire", "role": "assistant", "content": "With event", "created_at": "2026-01-01T00:00:02Z", "events": []any{map[string]any{"seq": 1, "type": "text", "content": "With event"}}},
		}
		report["warnings"] = nil
		result := testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(requestID, report)).Want(http.StatusOK).Map()["result"].(map[string]any)
		warnings, ok := result["warnings"].([]any)
		if !ok || len(warnings) != 0 {
			t.Fatalf("nil private warnings projected as %T %v, want []", result["warnings"], result["warnings"])
		}
	})

	t.Run("actual user timeline event remains rejected", func(t *testing.T) {
		f := newWorkflowFixture(t)
		ref := f.listSource(t, "wire-revision-user-events")
		requestID, _, report := f.beginImport(t, ref, "wire-revision-user-events")
		report["messages"] = []any{
			map[string]any{"native_id": "user-with-event", "role": "user", "content": "Question", "created_at": "2026-01-01T00:00:00Z", "events": []any{map[string]any{"seq": 1, "type": "text", "content": "not a user event"}}},
		}
		report["warnings"] = []any{}
		testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(requestID, report)).Want(http.StatusConflict)
	})
}

func TestAgentWorkflowLifecycle_ProjectsActualDeliveryResults(t *testing.T) {
	t.Run("accepted steer supplies the canonical message id on replay", func(t *testing.T) {
		f := newWorkflowFixture(t)
		requestID := uuid.NewString()
		testutil.Call(t, testHandler.InitiateChatSteer, workflowUserRequest(t, http.MethodPost, map[string]any{
			"request_id": requestID, "task_id": f.task, "run_id": f.run, "turn_id": f.turn, "content": "actual daemon accepted steer",
		}, "sessionId", f.session)).Want(http.StatusAccepted)
		workflowLifecycleHeartbeat(t, f)
		first := testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(requestID, map[string]any{"delivery": "accepted"})).Want(http.StatusOK).Map()["result"].(map[string]any)
		messageID, ok := first["message_id"].(string)
		if !ok || messageID == "" {
			t.Fatalf("accepted private delivery did not receive public message_id: %v", first)
		}
		var storedID string
		dbfx.QueryRow(t, `SELECT id FROM chat_message WHERE input_request_id=$1`, requestID).Scan(&storedID)
		if storedID != messageID {
			t.Fatalf("public message_id=%q, canonical chat row=%q", messageID, storedID)
		}
		replayed := testutil.Call(t, testHandler.ReportAgentWorkflowResult, f.reportRequest(requestID, map[string]any{"delivery": "accepted"})).Want(http.StatusOK).Map()["result"].(map[string]any)
		if replayed["message_id"] != messageID {
			t.Fatalf("duplicate accepted delivery changed message id: first=%v replay=%v", first, replayed)
		}
	})

	t.Run("nonaccepted deliveries explicitly have no message id", func(t *testing.T) {
		f := newWorkflowFixture(t)
		requestID := uuid.NewString()
		testutil.Call(t, testHandler.InitiateChatSteer, workflowUserRequest(t, http.MethodPost, map[string]any{
			"request_id": requestID, "task_id": f.task, "run_id": f.run, "turn_id": f.turn, "content": "actual daemon rejected steer",
		}, "sessionId", f.session)).Want(http.StatusAccepted)
		workflowLifecycleHeartbeat(t, f)
		result := testutil.Call(t, testHandler.ReportAgentWorkflowResult, testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/result", map[string]any{
			"status": "unknown", "result": map[string]any{"delivery": "unknown", "code": "transport_lost"},
			"error": map[string]any{"code": "transport_lost", "message": "provider delivery could not be confirmed"},
		}, testWorkspaceID, f.daemon), "runtimeId", f.runtime, "requestId", requestID)).Want(http.StatusOK).Map()["result"].(map[string]any)
		if value, exists := result["message_id"]; !exists || value != nil {
			t.Fatalf("nonaccepted delivery message_id = %v (present=%t), want explicit null", value, exists)
		}
	})
}

func TestAgentWorkflowLifecycle_TeammateControlsRemainAuthorizedWhileNativeHistoryDoesNot(t *testing.T) {
	f := newWorkflowFixture(t)
	teammateID := dbfx.User(t, "workflow teammate", "workflow-teammate-"+uuid.NewString()+"@example.test")
	dbfx.Member(t, testWorkspaceID, teammateID, "member")
	dbfx.Exec(t, `UPDATE agent SET permission_mode='public_to' WHERE id=$1`, f.agent)
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'member', $2)`, f.agent, teammateID)
	dbfx.Exec(t, `UPDATE chat_session SET creator_id=$1 WHERE id=$2`, teammateID, f.session)

	testutil.Call(t, testHandler.GetAgentWorkflowCapabilities, workflowLifecycleUserRequestAs(t, teammateID, http.MethodGet, nil, "runtimeId", f.runtime)).Want(http.StatusOK)
	interactionID := f.lifecycleApproval(t)
	body := f.answerBody(map[string]any{"choice_id": "allow_once"})
	testutil.Call(t, testHandler.RespondChatInteraction, workflowLifecycleUserRequestAs(t, teammateID, http.MethodPost, body, "sessionId", f.session, "interactionId", interactionID)).Want(http.StatusAccepted)
	testutil.Call(t, testHandler.InitiateNativeSessionList, workflowLifecycleUserRequestAs(t, teammateID, http.MethodPost, map[string]any{"request_id": uuid.NewString(), "limit": 20}, "runtimeId", f.runtime)).Want(http.StatusForbidden)
}

func TestAgentWorkflowLifecycle_PollingRechecksOperationAuthorityAndRuntime(t *testing.T) {
	t.Run("wrong runtime and revoked native ownership", func(t *testing.T) {
		f := newWorkflowFixture(t)
		requestID := uuid.NewString()
		testutil.Call(t, testHandler.InitiateNativeSessionList, workflowUserRequest(t, http.MethodPost, map[string]any{"request_id": requestID, "limit": 20}, "runtimeId", f.runtime)).Want(http.StatusAccepted)
		testutil.Call(t, testHandler.GetAgentWorkflowRequest, workflowUserRequest(t, http.MethodGet, nil, "runtimeId", uuid.NewString(), "requestId", requestID)).Want(http.StatusNotFound)
		newOwner := dbfx.User(t, "workflow native new owner", "workflow-native-owner-"+uuid.NewString()+"@example.test")
		dbfx.Exec(t, `UPDATE agent_runtime SET owner_id=$1 WHERE id=$2`, newOwner, f.runtime)
		testutil.Call(t, testHandler.GetAgentWorkflowRequest, workflowUserRequest(t, http.MethodGet, nil, "runtimeId", f.runtime, "requestId", requestID)).Want(http.StatusForbidden)
	})

	t.Run("revoked chat invocation", func(t *testing.T) {
		f := newWorkflowFixture(t)
		requestID := uuid.NewString()
		testutil.Call(t, testHandler.InitiateChatSteer, workflowUserRequest(t, http.MethodPost, map[string]any{
			"request_id": requestID, "task_id": f.task, "run_id": f.run, "turn_id": f.turn, "content": "poll after revocation",
		}, "sessionId", f.session)).Want(http.StatusAccepted)
		newOwner := dbfx.User(t, "workflow control new owner", "workflow-control-owner-"+uuid.NewString()+"@example.test")
		dbfx.Exec(t, `UPDATE agent SET owner_id=$1, permission_mode='public_to' WHERE id=$2`, newOwner, f.agent)
		testutil.Call(t, testHandler.GetAgentWorkflowRequest, workflowUserRequest(t, http.MethodGet, nil, "runtimeId", f.runtime, "requestId", requestID)).Want(http.StatusForbidden)
	})
}
