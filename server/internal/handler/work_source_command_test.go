package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file tests the read-only source command slice: allowlist create
// guards, idempotent duplicate receipts, owner-routed claim/report with
// CAS state transitions, and member receipt reads. No real bd CLI, no
// installed agent executable: the daemon side is represented only by fake
// runtime rows, matching the default-test contract.

func workSourceCommandHandler() *WorkSourceCommandHandler {
	return &WorkSourceCommandHandler{
		Handler:  testHandler,
		Commands: service.NewWorkSourceCommandService(testHandler.Queries, testPool),
	}
}

// createSourceCommandViaAPI posts one command and returns the decoded
// receipt. Terminal receipts are swept via t.Cleanup before the source
// cascade cleanup (LIFO).
func createSourceCommandViaAPI(t *testing.T, h *WorkSourceCommandHandler, status int, workspaceID, userID, sourceID string, body map[string]any) map[string]any {
	t.Helper()
	if _, ok := body["request_id"]; !ok {
		body["request_id"] = uuid.NewString()
	}
	req := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodPost, "/api/work-sources/"+sourceID+"/commands", body),
		"X-Workspace-ID", workspaceID,
		"X-User-ID", userID,
	)
	req = testutil.WithURLParams(req, "sourceID", sourceID)
	var resp map[string]any
	testutil.Call(t, h.CreateWorkSourceCommand, req).Want(status).JSON(&resp)
	if id, ok := resp["id"].(string); ok {
		t.Cleanup(func() {
			dbfx.Exec(t, `DELETE FROM work_source_command WHERE id = $1`, id)
		})
	}
	return resp
}

// Claim/report use the shared runtime/workspace access helper and explicit
// daemon middleware context to prove the caller's machine identity.

func claimCommandViaAPI(t *testing.T, h *WorkSourceCommandHandler, status int, runtimeID, commandID string) map[string]any {
	t.Helper()
	req := testutil.WithURLParams(
		testutil.JSONRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/work-source-commands/"+commandID+"/claim", nil),
		"runtimeId", runtimeID, "commandId", commandID,
	)
	// Simulate authenticated daemon middleware, not user-token fallback.
	req.Header.Set("X-User-ID", ownerUserIDForRuntime(t, runtimeID))
	req = commandDaemonContext(t, req, runtimeID)
	var resp map[string]any
	testutil.Call(t, h.ClaimWorkSourceCommand, req).Want(status).JSON(&resp)
	return resp
}

func reportCommandViaAPI(t *testing.T, h *WorkSourceCommandHandler, status int, runtimeID, commandID string, body map[string]any) map[string]any {
	t.Helper()
	req := testutil.WithURLParams(
		testutil.JSONRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/work-source-commands/"+commandID+"/result", body),
		"runtimeId", runtimeID, "commandId", commandID,
	)
	req.Header.Set("X-User-ID", ownerUserIDForRuntime(t, runtimeID))
	req = commandDaemonContext(t, req, runtimeID)
	var resp map[string]any
	testutil.Call(t, h.ReportWorkSourceCommand, req).Want(status).JSON(&resp)
	return resp
}

func TestWorkSourceCommandCreateAllowlistAndGuards(t *testing.T) {
	h := workSourceCommandHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "cmd")
	source := createSourceViaAPI(t, &WorkSourceHandler{
		Handler: testHandler, WorkSources: service.NewWorkSourceService(testHandler.Queries, testPool),
	}, workspaceID, userID, runtimeID, "handle-cmd-allow")
	sourceID := source["id"].(string)

	// Non-allowlisted command strings are rejected before any DB write.
	createSourceCommandViaAPI(t, h, http.StatusBadRequest, workspaceID, userID, sourceID,
		map[string]any{"command": "shell", "native_id": "x"})
	createSourceCommandViaAPI(t, h, http.StatusBadRequest, workspaceID, userID, sourceID,
		map[string]any{"command": "exec", "native_id": "x"})
	// read without native_id is invalid.
	createSourceCommandViaAPI(t, h, http.StatusBadRequest, workspaceID, userID, sourceID,
		map[string]any{"command": "read"})
	// list limit out of bounds.
	createSourceCommandViaAPI(t, h, http.StatusBadRequest, workspaceID, userID, sourceID,
		map[string]any{"command": "list", "limit": 201})
	// member (non-admin) may not create.
	viewer := seedWorkSourceMemberViewer(t, workspaceID, "cmd")
	createSourceCommandViaAPI(t, h, http.StatusForbidden, workspaceID, viewer, sourceID,
		map[string]any{"command": "list"})
	// unknown source is 404.
	createSourceCommandViaAPI(t, h, http.StatusNotFound, workspaceID, userID, uuid.NewString(),
		map[string]any{"command": "list"})

	// Valid read command creates a pending receipt.
	receipt := createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID,
		map[string]any{"command": "read", "native_id": "bd-1"})
	if receipt["status"] != "pending" {
		t.Fatalf("want pending, got %v", receipt["status"])
	}
	if receipt["native_id"] != "bd-1" {
		t.Fatalf("want native_id bd-1, got %v", receipt["native_id"])
	}
	// Duplicate identical in-flight request is idempotent (200, same id).
	dup := createSourceCommandViaAPI(t, h, http.StatusOK, workspaceID, userID, sourceID,
		map[string]any{"command": "read", "native_id": "bd-1", "request_id": receipt["request_id"]})
	if dup["id"] != receipt["id"] {
		t.Fatalf("idempotent retry returned new id %v, want %v", dup["id"], receipt["id"])
	}
	// Different in-flight request on the same source is a conflict.
	createSourceCommandViaAPI(t, h, http.StatusConflict, workspaceID, userID, sourceID,
		map[string]any{"command": "list"})
}

func TestWorkSourceCommandDisabledSourceRefused(t *testing.T) {
	h := workSourceCommandHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "cmddis")
	wsHandler := &WorkSourceHandler{
		Handler: testHandler, WorkSources: service.NewWorkSourceService(testHandler.Queries, testPool),
	}
	source := createSourceViaAPI(t, wsHandler, workspaceID, userID, runtimeID, "handle-cmd-dis")
	sourceID := source["id"].(string)

	disabled := false
	patch := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodPatch, "/api/work-sources/"+sourceID,
			map[string]any{"name": "Disabled", "enabled": disabled}),
		"X-Workspace-ID", workspaceID, "X-User-ID", userID,
	)
	patch = testutil.WithURLParams(patch, "sourceID", sourceID)
	var patched map[string]any
	testutil.Call(t, wsHandler.UpdateWorkSource, patch).Want(http.StatusOK).JSON(&patched)

	createSourceCommandViaAPI(t, h, http.StatusConflict, workspaceID, userID, sourceID,
		map[string]any{"command": "list"})
}

func TestWorkSourceCommandOwnerRoutedClaimReport(t *testing.T) {
	h := workSourceCommandHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "cmdown")
	wsHandler := &WorkSourceHandler{
		Handler: testHandler, WorkSources: service.NewWorkSourceService(testHandler.Queries, testPool),
	}
	source := createSourceViaAPI(t, wsHandler, workspaceID, userID, runtimeID, "handle-cmd-own")
	sourceID := source["id"].(string)

	receipt := createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID,
		map[string]any{"command": "list", "limit": 10})
	commandID := receipt["id"].(string)

	// A runtime of a different daemon cannot claim: seed a second runtime
	// with a different daemon_id in the same workspace.
	otherRuntimeID := dbfx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": workspaceID,
		"daemon_id":    "daemon-other-" + fmt.Sprint(time.Now().UnixNano()),
		"name":         "other runtime",
		"runtime_mode": "cloud",
		"provider":     "handler_test_sibling_runtime",
		"status":       "online",
		"device_info":  "",
		"metadata":     testutil.Raw("'{}'::jsonb"),
		"visibility":   "private",
		"owner_id":     userID,
	})
	claimCommandViaAPI(t, h, http.StatusForbidden, otherRuntimeID, commandID)
	// A foreign daemon token cannot impersonate the addressed owner runtime.
	for _, daemonID := range []string{"", "foreign-daemon"} {
		req := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/claim", nil), "runtimeId", runtimeID, "commandId", commandID)
		req.Header.Set("X-User-ID", userID)
		if daemonID != "" {
			req = req.WithContext(middleware.WithDaemonContext(req.Context(), workspaceID, daemonID))
		}
		testutil.Call(t, h.ClaimWorkSourceCommand, req).Want(http.StatusForbidden)
	}
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id = (SELECT daemon_id FROM agent_runtime WHERE id = $1) WHERE id = $2`, runtimeID, otherRuntimeID)
	// Sharing a daemon does not make a sibling runtime the source runtime.
	claimCommandViaAPI(t, h, http.StatusForbidden, otherRuntimeID, commandID)

	// Owner runtime claims pending -> claimed.
	claimed := claimCommandViaAPI(t, h, http.StatusOK, runtimeID, commandID)
	if claimed["status"] != "claimed" {
		t.Fatalf("want claimed, got %v", claimed["status"])
	}
	if claimed["claimed_runtime_id"] != runtimeID {
		t.Fatalf("want claimed_runtime_id %s, got %v", runtimeID, claimed["claimed_runtime_id"])
	}
	// Double claim is not claimable.
	claimCommandViaAPI(t, h, http.StatusConflict, runtimeID, commandID)

	// Non-claimer runtime cannot report.
	reportCommandViaAPI(t, h, http.StatusForbidden, otherRuntimeID, commandID,
		map[string]any{"status": "succeeded", "result": "[]"})
	// Invalid status.
	reportCommandViaAPI(t, h, http.StatusBadRequest, runtimeID, commandID,
		map[string]any{"status": "weird"})
	// succeeded requires result.
	reportCommandViaAPI(t, h, http.StatusBadRequest, runtimeID, commandID,
		map[string]any{"status": "succeeded"})

	// Owner reports success with a bounded JSON result.
	for _, raw := range []string{"null", "{}", "42", "[] []", `[{"id":""}]`, `[{"id":"a"},{"id":"a"}]`} {
		reportCommandViaAPI(t, h, http.StatusBadRequest, runtimeID, commandID, map[string]any{"status": "succeeded", "result": raw})
	}
	foreignReport := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/report", map[string]any{"status": "succeeded", "result": "[]"}), "runtimeId", runtimeID, "commandId", commandID)
	foreignReport = foreignReport.WithContext(middleware.WithDaemonContext(foreignReport.Context(), workspaceID, "foreign-daemon"))
	testutil.Call(t, h.ReportWorkSourceCommand, foreignReport).Want(http.StatusForbidden)
	result := `[{"id":"bd-1","title":"one"}]`
	done := reportCommandViaAPI(t, h, http.StatusOK, runtimeID, commandID,
		map[string]any{"status": "succeeded", "result": result})
	if done["status"] != "succeeded" || done["result"] == "" {
		t.Fatalf("want succeeded with result, got %v / %v", done["status"], done["result"])
	}
	// Replay of the terminal report is idempotent and does not mutate.
	replay := reportCommandViaAPI(t, h, http.StatusOK, runtimeID, commandID,
		map[string]any{"status": "succeeded", "result": result})
	if replay["status"] != "succeeded" || replay["result"] != done["result"] {
		t.Fatalf("terminal replay mutated receipt: %v", replay)
	}
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, commandID, map[string]any{"status": "failed", "error": "late"})
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, commandID, map[string]any{"status": "succeeded", "result": "[]"})
	reportCommandViaAPI(t, h, http.StatusForbidden, otherRuntimeID, commandID, map[string]any{"status": "succeeded", "result": result})
	terminalRetry := createSourceCommandViaAPI(t, h, http.StatusOK, workspaceID, userID, sourceID, map[string]any{"request_id": receipt["request_id"], "command": "list", "limit": 10})
	if terminalRetry["id"] != commandID {
		t.Fatal("terminal request identity lost")
	}
	createSourceCommandViaAPI(t, h, http.StatusConflict, workspaceID, userID, sourceID, map[string]any{"request_id": receipt["request_id"], "command": "list", "limit": 11})

	// Member reads the receipt; viewer member can GET but not create.
	viewer := seedWorkSourceMemberViewer(t, workspaceID, "cmdown")
	getReq := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodGet, "/api/work-source-commands/"+commandID, nil),
		"X-Workspace-ID", workspaceID, "X-User-ID", viewer,
	)
	getReq = testutil.WithURLParams(getReq, "commandID", commandID)
	var got map[string]any
	testutil.Call(t, h.GetWorkSourceCommand, getReq).Want(http.StatusOK).JSON(&got)
	if got["id"] != commandID || got["status"] != "succeeded" {
		t.Fatalf("receipt read mismatch: %v", got)
	}

	// After the terminal receipt, a new command can be created (in-flight
	// slot freed) and failed, then listed.
	next := createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID,
		map[string]any{"command": "read", "native_id": "bd-2"})
	nextID := next["id"].(string)
	claimCommandViaAPI(t, h, http.StatusOK, runtimeID, nextID)
	reportCommandViaAPI(t, h, http.StatusOK, runtimeID, nextID,
		map[string]any{"status": "failed", "error": "source unreachable"})
	reportCommandViaAPI(t, h, http.StatusOK, runtimeID, nextID, map[string]any{"status": "failed", "error": "source unreachable"})
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, nextID, map[string]any{"status": "failed", "error": "changed failure"})
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, nextID, map[string]any{"status": "succeeded", "result": `{"id":"bd-2","revision":"r1"}`})
	listReq := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodGet, "/api/work-sources/"+sourceID+"/commands", nil),
		"X-Workspace-ID", workspaceID, "X-User-ID", userID,
	)
	listReq = testutil.WithURLParams(listReq, "sourceID", sourceID)
	var list []map[string]any
	testutil.Call(t, h.ListWorkSourceCommands, listReq).Want(http.StatusOK).JSON(&list)
	if len(list) != 2 {
		t.Fatalf("want 2 receipts, got %d", len(list))
	}
	for _, row := range list {
		if _, ok := row["result"]; ok {
			t.Fatal("history includes result body")
		}
	}
}

func commandDaemonContext(t *testing.T, r *http.Request, runtimeID string) *http.Request {
	t.Helper()
	var workspaceID, daemonID string
	if err := testPool.QueryRow(context.Background(), `SELECT workspace_id,daemon_id FROM agent_runtime WHERE id=$1`, runtimeID).Scan(&workspaceID, &daemonID); err != nil {
		t.Fatal(err)
	}
	return r.WithContext(middleware.WithDaemonContext(r.Context(), workspaceID, daemonID))
}

func TestWorkSourceCommandRevisionDisabledAndExpired(t *testing.T) {
	h := workSourceCommandHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "cmdfence")
	source := createSourceViaAPI(t, &WorkSourceHandler{Handler: testHandler, WorkSources: service.NewWorkSourceService(testHandler.Queries, testPool)}, workspaceID, userID, runtimeID, "cmd-fences")
	sourceID := source["id"].(string)
	pending := createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID, map[string]any{"command": "list"})
	id := pending["id"].(string)
	dbfx.Exec(t, `UPDATE work_source SET enabled=false WHERE id=$1`, sourceID)
	claimCommandViaAPI(t, h, http.StatusConflict, runtimeID, id)
	dbfx.Exec(t, `UPDATE work_source SET enabled=true, config_revision=config_revision+1 WHERE id=$1`, sourceID)
	claimCommandViaAPI(t, h, http.StatusConflict, runtimeID, id)
	dbfx.Exec(t, `UPDATE work_source SET config_revision=config_revision-1 WHERE id=$1`, sourceID)
	dbfx.Exec(t, `UPDATE agent_runtime SET status='offline' WHERE id=$1`, runtimeID)
	claimCommandViaAPI(t, h, http.StatusForbidden, runtimeID, id)
	dbfx.Exec(t, `UPDATE agent_runtime SET status='online' WHERE id=$1`, runtimeID)
	dbfx.Exec(t, `UPDATE work_source_command SET expires_at=now()-interval '1 second' WHERE id=$1`, id)
	claimCommandViaAPI(t, h, http.StatusConflict, runtimeID, id)
	n, err := h.Commands.ExpireWorkSourceCommands(context.Background(), 10)
	if err != nil || n < 1 {
		t.Fatalf("pending expiry n=%d err=%v", n, err)
	}
	var status, diagnostic string
	if err := testPool.QueryRow(context.Background(), `SELECT status,error FROM work_source_command WHERE id=$1`, id).Scan(&status, &diagnostic); err != nil || status != "failed" || diagnostic == "" {
		t.Fatalf("expiry diagnostic %s %q %v", status, diagnostic, err)
	}
	claimed := createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID, map[string]any{"command": "read", "native_id": "a"})
	id = claimed["id"].(string)
	claimCommandViaAPI(t, h, http.StatusOK, runtimeID, id)
	dbfx.Exec(t, `UPDATE work_source SET enabled=false WHERE id=$1`, sourceID)
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, id, map[string]any{"status": "succeeded", "result": `{"id":"a","revision":"r1"}`})
	dbfx.Exec(t, `UPDATE work_source SET enabled=true,config_revision=config_revision+1 WHERE id=$1`, sourceID)
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, id, map[string]any{"status": "succeeded", "result": `{"id":"a","revision":"r1"}`})
	dbfx.Exec(t, `UPDATE work_source SET config_revision=config_revision-1 WHERE id=$1`, sourceID)
	for _, raw := range []string{"null", "[]", `{"id":"b","revision":"r1"}`, `{"id":"a"}`, `{"id":"a","revision":"r1"} {}`} {
		reportCommandViaAPI(t, h, http.StatusBadRequest, runtimeID, id, map[string]any{"status": "succeeded", "result": raw})
	}
	dbfx.Exec(t, `UPDATE work_source_command SET expires_at=now()-interval '1 second' WHERE id=$1`, id)
	reportCommandViaAPI(t, h, http.StatusConflict, runtimeID, id, map[string]any{"status": "succeeded", "result": `{"id":"a","revision":"r1"}`})
	if _, err := h.Commands.ExpireWorkSourceCommands(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	reportCommandViaAPI(t, h, http.StatusForbidden, runtimeID, id, map[string]any{"status": "failed", "error": "Read command expired before completion"})
	createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID, map[string]any{"command": "list"})
}

func ownerUserIDForRuntime(t *testing.T, runtimeID string) string {
	t.Helper()
	var ownerID string
	err := testPool.QueryRow(context.Background(),
		`SELECT owner_id FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&ownerID)
	if err != nil {
		t.Fatalf("load runtime owner: %v", err)
	}
	return ownerID
}

func TestWorkSourceCommandWorkspaceRuntimeLockOrder(t *testing.T) {
	for _, operation := range []string{"claim", "report"} {
		t.Run(operation, func(t *testing.T) {
			h := workSourceCommandHandler()
			workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "cmdlock"+operation)
			source := createSourceViaAPI(t, &WorkSourceHandler{Handler: testHandler, WorkSources: service.NewWorkSourceService(testHandler.Queries, testPool)}, workspaceID, userID, runtimeID, "cmd-lock-"+operation)
			sourceID := source["id"].(string)
			receipt := createSourceCommandViaAPI(t, h, http.StatusCreated, workspaceID, userID, sourceID, map[string]any{"command": "list"})
			commandID := receipt["id"].(string)
			if operation == "report" {
				claimCommandViaAPI(t, h, http.StatusOK, runtimeID, commandID)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			runtime, err := testHandler.Queries.GetAgentRuntime(ctx, parseUUID(runtimeID))
			if err != nil {
				t.Fatal(err)
			}
			workspaceConn, err := testPool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer workspaceConn.Release()
			workspaceTx, err := workspaceConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer workspaceTx.Rollback(context.Background())
			// Match DeleteWorkspace's runtime-preparation lock, before sources.
			if _, err := workspaceTx.Exec(ctx, `SELECT id FROM agent_runtime WHERE id=$1 FOR UPDATE`, runtimeID); err != nil {
				t.Fatal(err)
			}
			workerConn, err := testPool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer workerConn.Release()
			worker := service.NewWorkSourceCommandService(db.New(workerConn), workerConn)
			done := make(chan error, 1)
			workerDone := make(chan struct{})
			workerPID := int32(workerConn.Conn().PgConn().PID())
			workspacePID := int32(workspaceConn.Conn().PgConn().PID())
			go func() {
				defer close(workerDone)
				if operation == "claim" {
					_, err := worker.ClaimWorkSourceCommand(ctx, parseUUID(workspaceID), parseUUID(commandID), runtime)
					done <- err
				} else {
					_, err := worker.ReportWorkSourceCommand(ctx, service.ReportWorkSourceCommandParams{WorkspaceID: parseUUID(workspaceID), CommandID: parseUUID(commandID), Runtime: runtime, Status: "succeeded", Result: pgtype.Text{String: "[]", Valid: true}})
					done <- err
				}
			}()
			// Always release the blocking transaction and join the worker before
			// returning its pinned connection, including assertion failures.
			defer func() {
				workspaceTx.Rollback(context.Background())
				cancel()
				select {
				case <-workerDone:
				case <-time.After(2 * time.Second):
					t.Error("command worker did not stop")
				}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := testPool.QueryRow(ctx, `SELECT $2::int = ANY(pg_blocking_pids($1::int))`, workerPID, workspacePID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal("command never blocked on workspace runtime lock")
				}
			}
			// Once PostgreSQL proves the command is blocked on runtime, neither
			// source nor command may already be locked by it. The old reverse
			// order fails NOWAIT here deterministically, without a sleep race.
			if _, err := workspaceTx.Exec(ctx, `SELECT id FROM work_source WHERE id=$1 FOR UPDATE NOWAIT`, sourceID); err != nil {
				t.Fatalf("command locked source before runtime: %v", err)
			}
			if _, err := workspaceTx.Exec(ctx, `SELECT id FROM work_source_command WHERE id=$1 FOR UPDATE NOWAIT`, commandID); err != nil {
				t.Fatalf("command locked receipt before runtime: %v", err)
			}
			if err := workspaceTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("command after workspace locks released: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("command did not finish after workspace locks released")
			}
		})
	}
}
