package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/beads"
)

// Exercise the installed daemon transport against the production router and
// PostgreSQL, rather than a mock HTTP contract. No agent or source executable
// is launched here. Dispatch has its own acceptance check.
func TestSourceReadClientThroughProductionRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, parentKind := range []string{"jwt", "pat"} {
		t.Run(parentKind, func(t *testing.T) {
			fx := testutil.New(testPool, testWorkspaceID, testUserID)
			runtimeID, daemonID := sourceReadFixture(t, fx, testUserID, "client source", "source-client-"+uuid.NewString())
			var sourceID, handle string
			if err := testPool.QueryRow(ctx, `SELECT id::text,source_handle FROM work_source WHERE runtime_id=$1 AND workspace_id=$2`, runtimeID, testWorkspaceID).Scan(&sourceID, &handle); err != nil {
				t.Fatal(err)
			}
			parent := testToken
			if parentKind == "pat" {
				parent = sourceReadPAT(t, fx, testUserID)
			}
			client := daemon.NewClient(testServer.URL)
			client.SetToken(parent)
			credential, err := client.ExchangeSourceReadToken(ctx, runtimeID, testWorkspaceID, daemonID)
			if err != nil {
				t.Fatalf("owner capability exchange: %v", err)
			}
			if client.Token() != parent {
				t.Fatal("capability exchange replaced human credential")
			}
			rows, err := client.ListSourceReadCommands(ctx, runtimeID, testWorkspaceID, credential.Token)
			if err != nil || len(rows) != 0 {
				t.Fatalf("empty pending list: rows=%d err=%v", len(rows), err)
			}
			requestID := uuid.NewString()
			body, err := json.Marshal(map[string]any{"request_id": requestID, "command": "list", "limit": 2})
			if err != nil {
				t.Fatal(err)
			}
			_, data := mustSourceReadCall(t, ctx, http.MethodPost, "/api/work-sources/"+sourceID+"/commands", parent, testWorkspaceID, string(body), http.StatusCreated)
			var receipt struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			rows, err = client.ListSourceReadCommands(ctx, runtimeID, testWorkspaceID, credential.Token)
			if err != nil || len(rows) != 1 {
				t.Fatalf("pending discovery: rows=%d err=%v", len(rows), err)
			}
			pending := rows[0]
			if pending.ID != receipt.ID || pending.RequestID != requestID || pending.SourceID != sourceID || pending.SourceHandle != handle || pending.WorkspaceID != testWorkspaceID || pending.Status != "pending" || pending.LimitCount != 2 {
				t.Fatal("pending transport changed command identity or execution contract")
			}
			claimed, err := client.ClaimSourceReadCommand(ctx, runtimeID, testWorkspaceID, receipt.ID, credential.Token)
			if err != nil || claimed.Status != "claimed" || claimed.ClaimedRuntimeID != runtimeID || claimed.RequestID != requestID {
				t.Fatalf("claim transport: status=%q err=%v", claimed.Status, err)
			}
			if _, err := client.ClaimSourceReadCommand(ctx, runtimeID, testWorkspaceID, receipt.ID, credential.Token); err == nil {
				t.Fatal("second claim unexpectedly succeeded")
			}
			for i := 0; i < 2; i++ {
				terminal, err := client.ReportSourceReadCommand(ctx, runtimeID, testWorkspaceID, receipt.ID, credential.Token, "succeeded", "[]", "")
				if err != nil || terminal.Status != "succeeded" || terminal.Result != "[]" {
					t.Fatalf("report/replay %d: status=%q err=%v", i, terminal.Status, err)
				}
			}
			if _, err := client.ReportSourceReadCommand(ctx, runtimeID, testWorkspaceID, receipt.ID, credential.Token, "failed", "", "opposite outcome"); err == nil {
				t.Fatal("opposite terminal outcome unexpectedly succeeded")
			}
			rows, err = client.ListSourceReadCommands(ctx, runtimeID, testWorkspaceID, credential.Token)
			if err != nil || len(rows) != 0 {
				t.Fatalf("terminal receipt remained pending: rows=%d err=%v", len(rows), err)
			}
			// Match the installed executor's typed JSON output. The server
			// canonicalizes this same type before storing and echoing it.
			detailResult, err := json.Marshal(beads.Issue{IssueSummary: beads.IssueSummary{ID: "bd-1"}, Revision: "r1"})
			if err != nil {
				t.Fatal(err)
			}
			for _, outcome := range []struct{ status, result, diagnostic string }{
				{"succeeded", string(detailResult), ""},
				{"failed", "", "Source read unavailable."},
			} {
				_, detailBody := mustSourceReadCall(t, ctx, http.MethodPost, "/api/work-sources/"+sourceID+"/commands", parent, testWorkspaceID,
					`{"request_id":"`+uuid.NewString()+`","command":"read","native_id":"bd-1"}`, http.StatusCreated)
				var detail struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(detailBody, &detail); err != nil {
					t.Fatal(err)
				}
				rows, err := client.ListSourceReadCommands(ctx, runtimeID, testWorkspaceID, credential.Token)
				if err != nil || len(rows) != 1 || rows[0].ID != detail.ID || rows[0].Command != "read" || rows[0].NativeID != "bd-1" || rows[0].LimitCount != 0 {
					t.Fatalf("detail discovery: rows=%d err=%v", len(rows), err)
				}
				if _, err := client.ClaimSourceReadCommand(ctx, runtimeID, testWorkspaceID, detail.ID, credential.Token); err != nil {
					t.Fatalf("detail claim: %v", err)
				}
				terminal, err := client.ReportSourceReadCommand(ctx, runtimeID, testWorkspaceID, detail.ID, credential.Token, outcome.status, outcome.result, outcome.diagnostic)
				if err != nil || terminal.Status != outcome.status || terminal.Result != outcome.result || terminal.Error != outcome.diagnostic {
					t.Fatalf("detail outcome transport: status=%q err=%v", terminal.Status, err)
				}
			}
			if parentKind == "pat" {
				fx.Exec(t, `UPDATE personal_access_token SET revoked=TRUE WHERE token_hash=$1`, auth.HashToken(parent))
				if _, err := client.ListSourceReadCommands(ctx, runtimeID, testWorkspaceID, credential.Token); err == nil {
					t.Fatal("revoked parent still authorized actual client")
				}
			}
			if client.Token() != parent {
				t.Fatal("scoped operation mutated human credential")
			}
		})
	}
}
