package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Exercise the production router and auth middleware over HTTP, not a
// handler call with test-injected daemon identity. This qualifies receipt
// transport and authorization, not automatic daemon source execution.
func TestWorkSourceCommandReceiptsThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixtures := testutil.New(testPool, testWorkspaceID, testUserID)
	daemonID := "source-router-" + uuid.NewString()
	runtimeID := fixtures.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": testWorkspaceID, "owner_id": testUserID,
		"name": daemonID, "daemon_id": daemonID, "provider": "source-router-test",
		"runtime_mode": "cloud", "status": "online", "visibility": "private",
		"device_info": "", "metadata": testutil.Raw("'{}'::jsonb"),
	})
	sourceID := fixtures.Insert(t, "work_source", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"runtime_id": runtimeID, "daemon_id": daemonID, "name": "Router source",
		"source_handle": "router-approved-handle", "mode": "observe",
	})
	mintToken := func(owner string) string {
		token, err := auth.GenerateDaemonToken()
		if err != nil {
			t.Fatal(err)
		}
		fixtures.Insert(t, "daemon_token", testutil.Cols{
			"token_hash": auth.HashToken(token), "workspace_id": testWorkspaceID,
			"daemon_id": owner, "expires_at": time.Now().Add(time.Hour),
		})
		return token
	}
	ownerToken, foreignToken := mintToken(daemonID), mintToken("foreign-"+daemonID)
	call := func(method, path, token, workspace string, body any, status int) []byte {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, method, testServer.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", workspace)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := testServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, resp.StatusCode, status, payload)
		}
		return payload
	}
	commandPath := "/api/work-sources/" + sourceID + "/commands"
	requestID := uuid.NewString()
	request := map[string]any{"request_id": requestID, "command": "list", "limit": 2}
	call(http.MethodPost, commandPath, "", testWorkspaceID, request, http.StatusUnauthorized)
	call(http.MethodPost, commandPath, testToken, testWorkspaceID, map[string]any{"request_id": "bad", "command": "list"}, http.StatusBadRequest)
	call(http.MethodPost, commandPath, testToken, testWorkspaceID, map[string]any{"request_id": uuid.NewString(), "command": "exec"}, http.StatusBadRequest)
	var receipt struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
		Result    string `json:"result"`
	}
	if err := json.Unmarshal(call(http.MethodPost, commandPath, testToken, testWorkspaceID, request, http.StatusCreated), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ID == "" || receipt.RequestID != requestID || receipt.Status != "pending" {
		t.Fatalf("invalid pending receipt: %+v", receipt)
	}
	commandID := receipt.ID
	call(http.MethodPost, commandPath, testToken, testWorkspaceID, request, http.StatusOK)
	foreignRuntime := fixtures.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": testWorkspaceID, "owner_id": testUserID,
		"name": "Other source runtime", "daemon_id": "other-" + daemonID,
		"provider": "other-source-router-test", "runtime_mode": "cloud",
		"status": "online", "device_info": "", "metadata": testutil.Raw("'{}'::jsonb"),
	})
	foreignSource := fixtures.Insert(t, "work_source", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"runtime_id": foreignRuntime, "daemon_id": "other-" + daemonID,
		"name": "Other pending source", "source_handle": "other-approved-handle", "mode": "observe",
	})
	fixtures.Insert(t, "work_source_command", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"source_id": foreignSource, "request_id": uuid.NewString(), "config_revision": 1,
		"command": "list", "status": "pending", "request_hash": "pending-other-runtime-test",
	})
	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	call(http.MethodGet, pendingPath, testToken, testWorkspaceID, nil, http.StatusForbidden)
	call(http.MethodGet, pendingPath, foreignToken, testWorkspaceID, nil, http.StatusForbidden)
	pending := func() []map[string]any {
		t.Helper()
		var rows []map[string]any
		if err := json.Unmarshal(call(http.MethodGet, pendingPath, ownerToken, testWorkspaceID, nil, http.StatusOK), &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := pending(); len(rows) != 1 || rows[0]["id"] != commandID || rows[0]["source_handle"] != "router-approved-handle" || rows[0]["result"] != nil {
		t.Fatalf("pending delivery lost scoped identity or leaked result: %v", rows)
	}
	fixtures.Exec(t, `UPDATE work_source SET enabled=false WHERE id=$1`, sourceID)
	if rows := pending(); len(rows) != 0 {
		t.Fatalf("disabled source delivered pending commands: %v", rows)
	}
	fixtures.Exec(t, `UPDATE work_source SET enabled=true, config_revision=config_revision+1 WHERE id=$1`, sourceID)
	if rows := pending(); len(rows) != 0 {
		t.Fatalf("stale configuration delivered pending commands: %v", rows)
	}
	fixtures.Exec(t, `UPDATE work_source SET config_revision=config_revision-1 WHERE id=$1`, sourceID)
	fixtures.Exec(t, `UPDATE agent_runtime SET status='offline' WHERE id=$1`, runtimeID)
	if rows := pending(); len(rows) != 0 {
		t.Fatalf("offline runtime delivered pending commands: %v", rows)
	}
	fixtures.Exec(t, `UPDATE agent_runtime SET status='online' WHERE id=$1`, runtimeID)
	fixtures.Exec(t, `UPDATE work_source_command SET expires_at=now()-interval '1 minute' WHERE id=$1`, commandID)
	if rows := pending(); len(rows) != 0 {
		t.Fatalf("expired command delivered as pending: %v", rows)
	}
	fixtures.Exec(t, `UPDATE work_source_command SET expires_at=now()+interval '5 minutes' WHERE id=$1`, commandID)
	claimPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands/" + commandID + "/claim"
	call(http.MethodPost, claimPath, testToken, testWorkspaceID, nil, http.StatusForbidden)
	call(http.MethodPost, claimPath, foreignToken, testWorkspaceID, nil, http.StatusForbidden)
	call(http.MethodPost, claimPath, ownerToken, testWorkspaceID, nil, http.StatusOK)
	if rows := pending(); len(rows) != 0 {
		t.Fatalf("claimed command redelivered as pending: %v", rows)
	}
	call(http.MethodPost, claimPath, ownerToken, testWorkspaceID, nil, http.StatusConflict)
	resultPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands/" + commandID + "/result"
	call(http.MethodPost, resultPath, foreignToken, testWorkspaceID, map[string]any{"status": "succeeded", "result": "[]"}, http.StatusForbidden)
	call(http.MethodPost, resultPath, ownerToken, testWorkspaceID, map[string]any{"status": "succeeded", "result": "null"}, http.StatusBadRequest)
	report := map[string]any{"status": "succeeded", "result": "[]"}
	call(http.MethodPost, resultPath, ownerToken, testWorkspaceID, report, http.StatusOK)
	call(http.MethodPost, resultPath, ownerToken, testWorkspaceID, report, http.StatusOK)
	call(http.MethodPost, resultPath, ownerToken, testWorkspaceID, map[string]any{"status": "failed", "error": "opposite replay"}, http.StatusConflict)
	call(http.MethodPost, commandPath, testToken, testWorkspaceID, request, http.StatusOK)
	if err := json.Unmarshal(call(http.MethodGet, "/api/work-source-commands/"+commandID, testToken, testWorkspaceID, nil, http.StatusOK), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ID != commandID || receipt.Status != "succeeded" || receipt.Result != "[]" {
		t.Fatalf("terminal receipt changed: %+v", receipt)
	}
	call(http.MethodGet, "/api/work-source-commands/"+commandID, testToken, uuid.NewString(), nil, http.StatusNotFound)
	foreignWorkspace := fixtures.Insert(t, "workspace", testutil.Cols{"name": "Foreign source commands", "slug": "foreign-command-" + uuid.NewString()})
	// Membership denial conceals the workspace just like an absent workspace.
	call(http.MethodGet, "/api/work-source-commands/"+commandID, testToken, foreignWorkspace, nil, http.StatusNotFound)
	var history []map[string]any
	if err := json.Unmarshal(call(http.MethodGet, commandPath, testToken, testWorkspaceID, nil, http.StatusOK), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0]["id"] != commandID || history[0]["result"] != nil {
		t.Fatalf("receipt history leaked result or lost identity: %v", history)
	}
	call(http.MethodDelete, "/api/work-sources/"+sourceID, testToken, testWorkspaceID, nil, http.StatusNoContent)
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source_command WHERE id=$1`, commandID); count != 0 {
		t.Fatalf("HTTP source deletion retained command: count=%d", count)
	}
}
