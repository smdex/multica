package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
)

// These checks exercise PostgreSQL's actual queue constraints, not a copied
// readiness model. They do not establish graph Start or daemon acceptance.
func TestGraphQueueSlotsPreserveUncertainExecution(t *testing.T) {
	pool := sharedTestPool(t)
	fx := dbfx.New(pool, "", "")
	fx.UserID = fx.User(t, "graph slots", uuid.NewString()+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "graph slots", "graph-slots-"+uuid.NewString())
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "graph slots")
	agentID := fx.Agent(t, "graph slots", runtimeID)
	sourceID, graphA, graphB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	makeTask := func(runID, source, nativeID, status string, uncertain bool) string {
		return fx.Task(t, agentID, dbfx.Cols{
			"runtime_id": runtimeID, "graph_run_id": runID,
			"work_source_id": source, "work_native_id": nativeID,
			"status": status, "execution_uncertain": uncertain,
			"originator_user_id": fx.UserID, "accountable_user_id": fx.UserID,
		})
	}
	a := makeTask(graphA, sourceID, "shared", "queued", false)
	b := makeTask(graphB, sourceID, "shared", "queued", false)
	fx.Exec(t, `UPDATE agent_task_queue SET status='dispatched' WHERE id=$1`, a)
	_, err := pool.Exec(context.Background(), `UPDATE agent_task_queue SET status='dispatched' WHERE id=$1`, b)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != "agent_task_native_active_slot" {
		t.Fatalf("cross-root shared work must reserve exactly one source slot: %v", err)
	}
	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='queued'`, b); got != 1 {
		t.Fatal("failed claim changed the competing queued attempt")
	}

	fx.Exec(t, `UPDATE agent_task_queue SET status='failed', execution_uncertain=TRUE WHERE id=$1`, a)
	_, err = pool.Exec(context.Background(), `UPDATE agent_task_queue SET status='dispatched' WHERE id=$1`, b)
	if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != "agent_task_native_active_slot" {
		t.Fatalf("failed status must not release an uncertain source slot: %v", err)
	}
	// Only a graph-aware stop-evidence transaction may perform this release in
	// production. The fixture explicitly clears it to check the index boundary.
	fx.Exec(t, `UPDATE agent_task_queue SET execution_uncertain=FALSE WHERE id=$1`, a)
	fx.Exec(t, `UPDATE agent_task_queue SET status='dispatched' WHERE id=$1`, b)
	makeTask(uuid.NewString(), uuid.NewString(), "shared", "running", false)
	makeTask(uuid.NewString(), sourceID, "other", "running", false)
	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND status IN ('dispatched','running')`, agentID); got != 3 {
		t.Fatalf("different source/item identities should remain independent, active=%d", got)
	}
}

func TestGraphQueueNodeSlotIncludesQueuedAndUncertainAttempts(t *testing.T) {
	pool := sharedTestPool(t)
	fx := dbfx.New(pool, "", "")
	fx.UserID = fx.User(t, "graph node slots", uuid.NewString()+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "graph node slots", "graph-node-slots-"+uuid.NewString())
	runtimeID := fx.Runtime(t, "graph node slots")
	agentID := fx.Agent(t, "graph node slots", runtimeID)
	graphID, sourceID := uuid.NewString(), uuid.NewString()
	taskID := fx.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "graph_run_id": graphID,
		"work_source_id": sourceID, "work_native_id": "node",
		"originator_user_id": fx.UserID, "accountable_user_id": fx.UserID,
	})
	for _, phase := range []string{"queued", "failed uncertain"} {
		if phase == "failed uncertain" {
			fx.Exec(t, `UPDATE agent_task_queue SET status='failed', execution_uncertain=TRUE WHERE id=$1`, taskID)
		}
		_, err := pool.Exec(context.Background(), `INSERT INTO agent_task_queue
			(agent_id,runtime_id,graph_run_id,work_source_id,work_native_id,status,originator_user_id,accountable_user_id)
			VALUES ($1,$2,$3,$4,'node','queued',$5,$5)`, agentID, runtimeID, graphID, sourceID, fx.UserID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != "agent_task_graph_node_slot" {
			t.Fatalf("%s graph node must keep its attempt slot: %v", phase, err)
		}
	}
	fx.Exec(t, `UPDATE agent_task_queue SET execution_uncertain=FALSE WHERE id=$1`, taskID)
	fx.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "graph_run_id": graphID,
		"work_source_id": sourceID, "work_native_id": "node",
		"originator_user_id": fx.UserID, "accountable_user_id": fx.UserID,
	})
}

func TestGraphQueueIdentityCannotMasqueradeAsLegacyKinds(t *testing.T) {
	pool := sharedTestPool(t)
	fx := dbfx.New(pool, "", "")
	fx.UserID = fx.User(t, "graph identity", uuid.NewString()+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "graph identity", "graph-identity-"+uuid.NewString())
	runtimeID := fx.Runtime(t, "graph identity")
	agentID := fx.Agent(t, "graph identity", runtimeID)
	issueID := fx.Issue(t, "legacy issue")
	chatID := fx.ChatSession(t, agentID)
	graphID, sourceID := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name      string
		graph     any
		source    any
		native    any
		runtime   any
		issue     any
		chat      any
		context   string
		uncertain bool
	}{
		{name: "partial identity", graph: graphID, runtime: runtimeID, context: "{}"},
		{name: "legacy uncertainty", runtime: runtimeID, context: "{}", uncertain: true},
		{name: "source without graph", source: sourceID, native: "node", runtime: runtimeID, context: "{}"},
		{name: "blank node", graph: graphID, source: sourceID, native: " ", runtime: runtimeID, context: "{}"},
		{name: "zero graph", graph: uuid.Nil.String(), source: sourceID, native: "node", runtime: runtimeID, context: "{}"},
		{name: "zero source", graph: graphID, source: uuid.Nil.String(), native: "node", runtime: runtimeID, context: "{}"},
		{name: "runtime missing", graph: graphID, source: sourceID, native: "node", context: "{}"},
		{name: "issue attached", graph: graphID, source: sourceID, native: "node", runtime: runtimeID, issue: issueID, context: "{}"},
		{name: "chat attached", graph: graphID, source: sourceID, native: "node", runtime: runtimeID, chat: chatID, context: "{}"},
		{name: "wakeup attached", graph: graphID, source: sourceID, native: "node", runtime: runtimeID, context: `{"wakeup_id":"` + uuid.NewString() + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), `INSERT INTO agent_task_queue
				(agent_id,runtime_id,graph_run_id,work_source_id,work_native_id,issue_id,chat_session_id,context,execution_uncertain,originator_user_id,accountable_user_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$10)`, agentID, tc.runtime, tc.graph, tc.source, tc.native, tc.issue, tc.chat, tc.context, tc.uncertain, fx.UserID)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "agent_task_graph_identity_check" {
				t.Fatalf("invalid graph identity should fail its specific constraint: %v", err)
			}
		})
	}
	// Unchanged legacy queue kinds remain legal after the additive migration.
	fx.Task(t, agentID, dbfx.Cols{"runtime_id": runtimeID, "originator_user_id": fx.UserID, "accountable_user_id": fx.UserID})
	fx.Task(t, agentID, dbfx.Cols{"runtime_id": runtimeID, "issue_id": issueID, "originator_user_id": fx.UserID, "accountable_user_id": fx.UserID})
	fx.Task(t, agentID, dbfx.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "originator_user_id": fx.UserID, "accountable_user_id": fx.UserID})
}
