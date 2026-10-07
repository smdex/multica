package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestWorkflowDraftDeletionLockOrderThroughRouter(t *testing.T) {
	for _, parent := range []string{"project", "workspace"} {
		t.Run(parent, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fx := testutil.New(testPool, testWorkspaceID, testUserID)
			project := fx.Project(t, "Draft deletion order")
			runtimeID, sourceID, revision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft delete order", "draft-order-"+uuid.NewString(), project)
			msr := sourceReadExchange(t, ctx, runtimeID, testToken)
			receiptID := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "order-root", draftCompleteItem("order-root", "r1"))
			_, raw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, draftBody(uuid.NewString(), sourceID, "order-root", "r1", int(revision), 1, []string{receiptID}), http.StatusCreated)
			var run workflowDraftRun
			if err := json.Unmarshal(raw, &run); err != nil {
				t.Fatal(err)
			}
			gate, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			lockSQL := "SELECT id FROM work_source WHERE id=$1 FOR UPDATE"
			lockID := sourceID
			path := "/api/projects/" + project
			probeSQL := "SELECT id FROM workflow_run WHERE id=$1 FOR UPDATE NOWAIT"
			probeID := run.ID
			if parent == "workspace" {
				// Never delete the shared workspace. Hold its lock and exercise
				// an actual source delete, proving it waits before taking S.
				lockSQL = "SELECT id FROM workspace WHERE id=$1 FOR UPDATE"
				lockID = testWorkspaceID
				path = "/api/work-sources/" + sourceID
				probeSQL = "SELECT id FROM work_source WHERE id=$1 FOR UPDATE NOWAIT"
				probeID = sourceID
			}
			if _, err := gate.Exec(ctx, lockSQL, lockID); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := gate.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			deleted := asyncSourceReadCall(ctx, http.MethodDelete, path, testToken, testWorkspaceID, "")
			if !waitForBlockedBy(ctx, testPool, blockerPID) {
				t.Fatal("deletion did not wait on its parent before descendant locks")
			}
			probe, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Rollback(context.Background())
			if _, err := probe.Exec(ctx, probeSQL, probeID); err != nil {
				t.Fatalf("deletion locked descendant before parent, allowing a lock cycle: %v", err)
			}
			if err := probe.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case status := <-deleted:
				if status != http.StatusNoContent {
					t.Fatalf("delete status %d, want 204", status)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if parent == "project" {
				detached := draftGet(t, ctx, testToken, testWorkspaceID, run.ID, http.StatusOK)
				if detached.ProjectID != "" {
					t.Fatal("project reference survived deletion")
				}
				mustSourceReadCall(t, ctx, http.MethodDelete, "/api/work-sources/"+sourceID, testToken, testWorkspaceID, "", http.StatusNoContent)
			}
			draftGet(t, ctx, testToken, testWorkspaceID, run.ID, http.StatusNotFound)
		})
	}
}
