package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Admission must re-read source eligibility after a real lock wait, not
// rely on the source snapshot available before the transaction blocks.
func TestWorkflowDraftSourceFenceAfterLockWaitThroughRouter(t *testing.T) {
	for _, change := range []string{"enabled=FALSE", "config_revision=config_revision+1"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fx := testutil.New(testPool, testWorkspaceID, testUserID)
			runtimeID, sourceID, revision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft source fence", "draft-fence-"+uuid.NewString(), "")
			msr := sourceReadExchange(t, ctx, runtimeID, testToken)
			receiptID := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "fence-root", draftCompleteItem("fence-root", "r1"))
			requestID := uuid.NewString()
			body := draftBody(requestID, sourceID, "fence-root", "r1", int(revision), 2, []string{receiptID})
			gate, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			if _, err := gate.Exec(ctx, "SELECT id FROM work_source WHERE id=$1 AND workspace_id=$2 FOR UPDATE", sourceID, testWorkspaceID); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := gate.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			result := asyncSourceReadCall(ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, body)
			if !waitForBlockedBy(ctx, testPool, blockerPID) {
				t.Fatal("draft admission did not reach the held source lock")
			}
			if _, err := gate.Exec(ctx, "UPDATE work_source SET "+change+" WHERE id=$1 AND workspace_id=$2", sourceID, testWorkspaceID); err != nil {
				t.Fatal(err)
			}
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case status := <-result:
				if status != http.StatusConflict {
					t.Fatalf("draft after eligibility changed while waiting: status %d, want 409", status)
				}
			case <-ctx.Done():
				t.Fatal("draft did not finish after source lock released")
			}
			if got := fx.Count(t, "SELECT count(*) FROM workflow_run WHERE workspace_id=$1 AND request_id=$2", testWorkspaceID, requestID); got != 0 {
				t.Fatalf("fenced draft inserted %d rows", got)
			}
		})
	}
}

// The real membership DELETE holds subscriber serialization while waiting
// on an owned runtime. Draft admission must wait behind it and see revocation.
func TestWorkflowDraftMembershipRevokeThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	actor := fx.User(t, "Draft revoked admin", "draft-revoke-"+uuid.NewString()+"@example.test")
	memberID := fx.Member(t, testWorkspaceID, actor, "admin")
	token, err := generateTestJWT(actor, "", "Draft revoked admin")
	if err != nil {
		t.Fatal(err)
	}
	runtimeID, sourceID, revision := draftSourceFixture(t, fx, testWorkspaceID, actor, "draft revoke runtime", "draft-revoke-"+uuid.NewString(), "")
	msr := sourceReadExchange(t, ctx, runtimeID, token)
	receiptID := draftReceipt(t, ctx, sourceID, runtimeID, msr, token, testWorkspaceID, "revoke-root", draftCompleteItem("revoke-root", "r1"))
	requestID := uuid.NewString()
	body := draftBody(requestID, sourceID, "revoke-root", "r1", int(revision), 2, []string{receiptID})
	gate, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(ctx, "SELECT id FROM agent_runtime WHERE id=$1 AND workspace_id=$2 FOR UPDATE", runtimeID, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	var gatePID int
	if err := gate.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	revoked := asyncSourceReadCall(ctx, http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/members/"+memberID, testToken, "", "")
	var revokePID int
	for revokePID == 0 {
		if err := testPool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' LIMIT 1),0)`, gatePID).Scan(&revokePID); err != nil {
			t.Fatal(err)
		}
		select {
		case status := <-revoked:
			t.Fatalf("membership DELETE escaped runtime gate: %d", status)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	admitted := asyncSourceReadCall(ctx, http.MethodPost, "/api/workflow-runs", token, testWorkspaceID, body)
	for {
		var waiting bool
		if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)) AND wait_event='advisory')`, revokePID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case status := <-admitted:
			t.Fatalf("draft escaped membership serialization: %d", status)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for label, result := range map[string]<-chan int{"membership DELETE": revoked, "draft": admitted} {
		want := http.StatusNoContent
		if label == "draft" {
			want = http.StatusNotFound
		}
		select {
		case status := <-result:
			if status != want {
				t.Fatalf("%s status %d, want %d", label, status, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if got := fx.Count(t, "SELECT count(*) FROM workflow_run WHERE workspace_id=$1 AND request_id=$2", testWorkspaceID, requestID); got != 0 {
		t.Fatalf("revoked admin inserted %d drafts", got)
	}
}
