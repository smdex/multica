package main

// Failing-first production-router regressions for the native graph deletion
// fence (bounded safety prerequisite, NOT graph activation):
//
// Until graph-aware retention/cleanup exists, ANY agent_task_queue row with a
// non-NULL graph_run_id conservatively blocks the destructive sweep that
// would otherwise orphan it:
//
//   - DELETE /api/work-sources/{id}: the source cascade currently deletes
//     workflow_run rows and the work_source itself, leaving graph tasks whose
//     graph_run_id/work_source_id point at deleted rows. The fence rejects
//     with 409 under the existing workspace+source lock, before any cleanup,
//     preserving source, workflow_run, issue_work_link and queue rows whole.
//   - DELETE /api/workspaces/{id}: teardown sweeps agent_task_queue through
//     agent/issue/runtime ownership, including rows whose liveness is
//     uncertain (execution_uncertain). The fence rejects with 409 after the
//     existing owner permission check and the workspace row lock, before any
//     destructive write.
//
// Predicates must match what each sweep actually deletes: the source fence
// blocks on exact work_source_id OR on graph_run_id referencing a workflow_run
// owned by the source being swept; the workspace fence blocks on source,
// agent, or runtime ownership OR on graph_run_id referencing a workflow_run
// owned by the workspace. Conservative retention must not orphan even
// mismatched (cross-owned/malformed) references; real application inserts
// will validate matching identities before activation.
//
// The fence is deliberately conservative: terminal-certain rows
// (completed/failed with execution_uncertain=FALSE) also block, because no
// graph-aware retention decision exists yet; the status matrices below keep
// that ceiling explicit.
//
// These tests stage red BEFORE the production change; the router returns the
// current behavior (204 sweep) until the fence ships. Fixtures insert real
// workflow_run rows (inert draft state, no imitation coordinator/scheduler);
// workflow_run is FK-free, so graph tasks reference it by UUID identity only.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// graphSourceFixture stages a runtime, an observe work_source and an agent in
// wsID, plus one draft workflow_run owned by the source. The source's daemon
// matches its runtime (coherent identity).
func graphSourceFixture(t *testing.T, fx *testutil.Fixture, wsID string) (runtimeID, sourceID, agentID, runID string) {
	t.Helper()
	// agent_runtime/agent/task have DEFAULT gen_random_uuid() ids; work_source
	// and workflow_run do NOT, so their ids are explicit here.
	daemonID := "ngd-" + uuid.NewString()
	runtimeID = fx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": wsID,
		"daemon_id":    daemonID,
		"name":         "ngd runtime",
		"provider":     "ngd", "runtime_mode": "local", "status": "online",
		"visibility": "private", "device_info": "",
		"metadata": testutil.Raw("'{}'::jsonb"), "owner_id": fx.UserID,
	})
	sourceID = fx.Insert(t, "work_source", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": wsID, "runtime_id": runtimeID,
		"daemon_id": daemonID, "name": "ngd source",
		"source_handle": "ngd-" + uuid.NewString(), "mode": "observe",
	})
	agentID = fx.Agent(t, "ngd agent", runtimeID, testutil.Cols{"workspace_id": wsID})
	runID = fx.Insert(t, "workflow_run", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": wsID, "source_id": sourceID,
		"request_id": uuid.NewString(), "request_hash": "ngd",
		"root_native_id":  "ngd-root-" + uuid.NewString(),
		"config_revision": 1, "capacity": 1,
		"graph":      testutil.Raw("'{}'::jsonb"),
		"node_state": testutil.Raw("'{}'::jsonb"), "created_by": fx.UserID,
	})
	return
}

// graphForeignFixture stages a fully foreign context (workspace, runtime,
// agent, source, own run) whose rows never belong to the workspace under
// test.
func graphForeignFixture(t *testing.T, fx *testutil.Fixture) (wsID, runtimeID, sourceID, agentID, runID string) {
	t.Helper()
	wsID = fx.Workspace(t, "ngd foreign ws", "ngd-foreign-"+uuid.NewString()[:8])
	runtimeID, sourceID, agentID, runID = graphSourceFixture(t, fx, wsID)
	return
}

// graphTask queues one graph-reserved task (graph_run_id identity, FK-free).
func graphTask(t *testing.T, fx *testutil.Fixture, agentID, runtimeID, sourceID, runID string, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"agent_id":   agentID,
		"status":     "queued",
		"runtime_id": runtimeID, "graph_run_id": runID,
		"work_source_id": sourceID, "work_native_id": "ngd-" + uuid.NewString(),
	}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	return fx.Insert(t, "agent_task_queue", cols)
}

// graphTaskJSON returns the whole task row as JSON for byte-stable
// before/after comparison (whole-row preservation, not selector spot checks).
func graphTaskJSON(t *testing.T, fx *testutil.Fixture, taskID string) string {
	t.Helper()
	var raw string
	fx.QueryRow(t, `SELECT to_jsonb(t) FROM agent_task_queue t WHERE id = $1`, taskID).Scan(&raw)
	return raw
}

func rowExists(t *testing.T, fx *testutil.Fixture, table, id string) bool {
	t.Helper()
	return fx.Count(t, `SELECT count(*) FROM `+table+` WHERE id = $1`, id) == 1
}

func dropGraphTask(t *testing.T, fx *testutil.Fixture, taskID string) {
	t.Helper()
	fx.Exec(t, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
}

// graphStatusMatrix is the shared retention-state matrix: every explicitly
// changed retention state blocks independently, including terminal-certain.
var graphStatusMatrix = []testutil.Cols{
	{"status": "queued"},
	{"status": "running"},
	{"status": "failed", "execution_uncertain": true},
	{"status": "failed", "execution_uncertain": false},
	{"status": "completed", "execution_uncertain": false},
}

// TestGraphDeletionFenceBlocksWorkSourceDelete pins the source-side fence:
// every graph reservation state blocks the cascade with 409 while preserving
// the complete source, workflow_run, issue_work_link and task rows; a
// cross-owned task (agent/runtime elsewhere, graph identity on this source)
// blocks; a graph-run-only reference (mismatched work_source_id, run owned by
// this source) blocks; genuinely foreign graph rows do not block; a graph-free
// source still deletes.
func TestGraphDeletionFenceBlocksWorkSourceDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	runtimeID, sourceID, agentID, runID := graphSourceFixture(t, fx, testWorkspaceID)
	issueID := fx.Issue(t, "ngd linked issue")
	linkID := fx.Insert(t, "issue_work_link", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": testWorkspaceID, "issue_id": issueID,
		"source_id": sourceID, "native_id": "ngd-" + uuid.NewString(),
	})
	deletePath := "/api/work-sources/" + sourceID

	// Authenticated denials: anonymous 401, plain member 403.
	plain := fx.User(t, "ngd member", "ngd-member-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, plain, "member")
	plainJWT, err := generateTestJWT(plain, "", "ngd member")
	if err != nil {
		t.Fatal(err)
	}
	mustSourceReadCall(t, ctx, "DELETE", deletePath, "", testWorkspaceID, "", http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, "DELETE", deletePath, plainJWT, testWorkspaceID, "", http.StatusForbidden)

	// Every reservation state conservatively blocks, including terminal
	// certain: no graph-aware retention decision exists yet.
	for _, st := range graphStatusMatrix {
		taskID := graphTask(t, fx, agentID, runtimeID, sourceID, runID, st)
		before := graphTaskJSON(t, fx, taskID)
		mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, testWorkspaceID, "", http.StatusConflict)
		if after := graphTaskJSON(t, fx, taskID); after != before {
			t.Fatalf("graph task row must be preserved byte-for-byte on 409 (status=%v)", st["status"])
		}
		if !rowExists(t, fx, "work_source", sourceID) || !rowExists(t, fx, "workflow_run", runID) || !rowExists(t, fx, "issue_work_link", linkID) {
			t.Fatalf("409 must preserve source, workflow_run and issue_work_link rows (status=%v)", st["status"])
		}
		dropGraphTask(t, fx, taskID)
	}

	// Foreign context for ownership legs below; only the workspace id is
	// unused here.
	_, fRuntime, fSource, fAgent, fRun := graphForeignFixture(t, fx)

	// Cross-owned: the task's agent/runtime live in another workspace, but
	// its work_source_id is this source; deleting the source would orphan the
	// reservation, so the exact-source arm blocks.
	crossTask := graphTask(t, fx, fAgent, fRuntime, sourceID, fRun)
	crossBefore := graphTaskJSON(t, fx, crossTask)
	mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, testWorkspaceID, "", http.StatusConflict)
	if graphTaskJSON(t, fx, crossTask) != crossBefore {
		t.Fatal("source-only 409 must preserve the graph task row")
	}
	if !rowExists(t, fx, "work_source", sourceID) {
		t.Fatal("cross-owned 409 must preserve the source row")
	}
	dropGraphTask(t, fx, crossTask)

	// Graph-run-only: queue-side identity is entirely foreign (mismatched
	// work_source_id), but graph_run_id references a run this sweep would
	// delete; the run-reference arm must block so referenced evidence is not
	// orphaned.
	runOnlyTask := graphTask(t, fx, fAgent, fRuntime, fSource, runID)
	mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, testWorkspaceID, "", http.StatusConflict)
	if !rowExists(t, fx, "work_source", sourceID) || !rowExists(t, fx, "workflow_run", runID) {
		t.Fatal("graph-run-only 409 must preserve the source and its run row")
	}
	dropGraphTask(t, fx, runOnlyTask)

	// Genuinely foreign graph rows (own run under a foreign source/workspace)
	// do not block an unrelated source.
	graphTask(t, fx, fAgent, fRuntime, fSource, fRun)

	// Graph-free (legacy-only) delete still succeeds.
	mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, testWorkspaceID, "", http.StatusNoContent)
	if rowExists(t, fx, "work_source", sourceID) || rowExists(t, fx, "workflow_run", runID) || rowExists(t, fx, "issue_work_link", linkID) {
		t.Fatal("graph-free source delete must still sweep source, run and link rows")
	}
	if !rowExists(t, fx, "work_source", fSource) || !rowExists(t, fx, "workflow_run", fRun) {
		t.Fatal("foreign graph rows must survive the unrelated source delete")
	}
}

// TestGraphDeletionFenceBlocksWorkspaceDelete pins the workspace-side fence:
// after the owner authority recheck and before any destructive write, any
// graph reservation reachable through the sweep's ownership paths (source,
// agent, runtime, or a workflow_run the sweep deletes — OR, matching the
// sweep) rejects with 409 and leaves the workspace wholly intact; each
// ownership leg is exercised independently so a missed predicate arm cannot
// hide; a graph-free workspace still deletes.
func TestGraphDeletionFenceBlocksWorkspaceDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	// Foreign context for the ownership legs; only the workspace id is
	// unused here.
	_, fRuntime, fSource, fAgent, fRun := graphForeignFixture(t, fx)

	blockedWS := fx.Workspace(t, "ngd blocked ws", "ngd-blocked-"+uuid.NewString()[:8])
	fx.Member(t, blockedWS, fx.UserID, "owner")
	runtimeID, sourceID, agentID, runID := graphSourceFixture(t, fx, blockedWS)

	// Same status matrix as the source-side fence.
	for _, st := range graphStatusMatrix {
		taskID := graphTask(t, fx, agentID, runtimeID, sourceID, runID, st)
		before := graphTaskJSON(t, fx, taskID)
		mustSourceReadCall(t, ctx, "DELETE", "/api/workspaces/"+blockedWS, testToken, blockedWS, "", http.StatusConflict)
		if after := graphTaskJSON(t, fx, taskID); after != before {
			t.Fatalf("graph task row must be preserved byte-for-byte on workspace 409 (status=%v)", st["status"])
		}
		if !rowExists(t, fx, "work_source", sourceID) || !rowExists(t, fx, "workflow_run", runID) {
			t.Fatalf("workspace 409 must preserve source and workflow_run rows (status=%v)", st["status"])
		}
		dropGraphTask(t, fx, taskID)
	}
	deletePath := "/api/workspaces/" + blockedWS

	// Keep one graph task live across the auth checks so the 409 below still
	// has a reservation behind it (the status loop drops each of its own).
	guard := graphTask(t, fx, agentID, runtimeID, sourceID, runID)
	guardBefore := graphTaskJSON(t, fx, guard)

	// Authenticated denials: anonymous 401, admin-not-owner 403.
	admin := fx.User(t, "ngd ws admin", "ngd-ws-admin-"+uuid.NewString()+"@example.test")
	fx.Member(t, blockedWS, admin, "admin")
	adminJWT, err := generateTestJWT(admin, "", "ngd ws admin")
	if err != nil {
		t.Fatal(err)
	}
	mustSourceReadCall(t, ctx, "DELETE", deletePath, "", blockedWS, "", http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, "DELETE", deletePath, adminJWT, blockedWS, "", http.StatusForbidden)

	mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, blockedWS, "", http.StatusConflict)
	if after := graphTaskJSON(t, fx, guard); after != guardBefore {
		t.Fatal("graph task row must be preserved byte-for-byte on workspace 409")
	}
	if !rowExists(t, fx, "workspace", blockedWS) || !rowExists(t, fx, "work_source", sourceID) ||
		!rowExists(t, fx, "workflow_run", runID) {
		t.Fatal("409 must preserve workspace, source and workflow_run rows")
	}
	dropGraphTask(t, fx, guard)

	// Independent ownership legs on a fresh workspace: each task is reachable
	// through exactly ONE sweep path, so a dropped predicate arm fails its
	// own leg instead of hiding behind another arm that also matches.
	legWS := fx.Workspace(t, "ngd leg ws", "ngd-leg-"+uuid.NewString()[:8])
	fx.Member(t, legWS, fx.UserID, "owner")
	legRuntime, legSource, legAgent, legRun := graphSourceFixture(t, fx, legWS)
	legPath := "/api/workspaces/" + legWS
	expectLegBlocked := func(taskID, leg string) {
		t.Helper()
		before := graphTaskJSON(t, fx, taskID)
		mustSourceReadCall(t, ctx, "DELETE", legPath, testToken, legWS, "", http.StatusConflict)
		if after := graphTaskJSON(t, fx, taskID); after != before {
			t.Fatalf("%s leg: graph task row must be preserved byte-for-byte on 409", leg)
		}
		if !rowExists(t, fx, "workspace", legWS) {
			t.Fatalf("%s leg 409 must preserve the workspace row", leg)
		}
		dropGraphTask(t, fx, taskID)
	}
	// Source-only: this workspace owns only the task's work_source; agent,
	// runtime and run are foreign, so exactly the source arm matches.
	expectLegBlocked(graphTask(t, fx, fAgent, fRuntime, legSource, fRun), "source-only")
	// Runtime-only: this workspace owns only the task's runtime.
	expectLegBlocked(graphTask(t, fx, fAgent, legRuntime, fSource, fRun), "runtime-only")
	// Agent-only: this workspace owns only the task's agent.
	expectLegBlocked(graphTask(t, fx, legAgent, fRuntime, fSource, fRun), "agent-only")
	// Graph-run-only: queue-side source/agent/runtime are all foreign, but
	// graph_run_id references a run in this workspace; the sweep would delete
	// the referenced run, orphaning the reservation.
	expectLegBlocked(graphTask(t, fx, fAgent, fRuntime, fSource, legRun), "graph-run-only")

	// With every leg row removed, the graph-free leg workspace still deletes
	// (also proving the fully foreign fixture rows never blocked it).
	mustSourceReadCall(t, ctx, "DELETE", legPath, testToken, legWS, "", http.StatusNoContent)
	if rowExists(t, fx, "workspace", legWS) {
		t.Fatal("graph-free leg workspace must still delete")
	}

	// Cross-owned: the task's agent belongs to this workspace while its graph
	// source belongs to another workspace; the sweep would delete the row via
	// agent ownership, so it must block.
	crossWS := fx.Workspace(t, "ngd crossowner ws", "ngd-crossowner-"+uuid.NewString()[:8])
	fx.Member(t, crossWS, fx.UserID, "owner")
	_, crossSource, _, crossRun := graphSourceFixture(t, fx, crossWS)
	crossTask := graphTask(t, fx, agentID, runtimeID, crossSource, crossRun)
	mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, blockedWS, "", http.StatusConflict)
	if !rowExists(t, fx, "workspace", blockedWS) {
		t.Fatal("cross-owned 409 must preserve the workspace row")
	}
	dropGraphTask(t, fx, crossTask)

	// Graph-free workspace (legacy tasks only) still deletes.
	legacyTask := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID})
	mustSourceReadCall(t, ctx, "DELETE", deletePath, testToken, blockedWS, "", http.StatusNoContent)
	if rowExists(t, fx, "workspace", blockedWS) || rowExists(t, fx, "agent_task_queue", legacyTask) {
		t.Fatal("graph-free workspace delete must still tear down workspace and legacy tasks")
	}
}

// TestGraphDeletionFenceSourceAfterLockWait proves the source-side retention
// check runs AFTER the source FOR UPDATE wait, not before: the graph fixture
// is inserted in a holder transaction that also holds the source lock, the
// public DELETE blocks behind it (verified via pg_blocking_pids), and the
// committed insert then yields 409 with rows intact. This pins placement only;
// it makes no claim about a future graph writer protocol or activation.
func TestGraphDeletionFenceSourceAfterLockWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	runtimeID, sourceID, agentID, runID := graphSourceFixture(t, fx, testWorkspaceID)
	deletePath := "/api/work-sources/" + sourceID

	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.Release()
		}
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var blockerPID int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_source WHERE id=$1 FOR UPDATE`, sourceID); err != nil {
		t.Fatal(err)
	}
	var holderTaskID string
	if err := tx.QueryRow(ctx, `INSERT INTO agent_task_queue
		(agent_id, status, runtime_id, graph_run_id, work_source_id, work_native_id)
		VALUES ($1, 'queued', $2, $3, $4, $5) RETURNING id`,
		agentID, runtimeID, runID, sourceID, "ngd-lock-"+uuid.NewString()).Scan(&holderTaskID); err != nil {
		t.Fatal(err)
	}
	fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE id = $1`, holderTaskID)

	done := asyncSourceReadCall(ctx, http.MethodDelete, deletePath, testToken, testWorkspaceID, "")
	if !waitForBlockedBy(ctx, testPool, blockerPID) {
		t.Fatal("source DELETE never blocked behind the held source lock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	committed = true
	conn.Release()
	if status := <-done; status != http.StatusConflict {
		t.Fatalf("source DELETE after lock wait + committed graph insert: status=%d want=409", status)
	}
	if !rowExists(t, fx, "work_source", sourceID) || !rowExists(t, fx, "workflow_run", runID) {
		t.Fatal("post-lock-wait 409 must preserve source and workflow_run rows")
	}
	if !rowExists(t, fx, "agent_task_queue", holderTaskID) {
		t.Fatal("holder-inserted graph task must survive the rejected delete")
	}
}

// TestGraphDeletionFenceWorkspaceAfterLockWait proves the workspace-side
// retention check runs AFTER the workspace FOR UPDATE wait: the holder takes
// FOR KEY SHARE on the workspace row (the create-side fence lock shape) and
// inserts a graph fixture inside the same transaction; the public DELETE
// blocks (pg_blocking_pids), the commit publishes the task, and the teardown
// answers 409 with the workspace wholly intact.
func TestGraphDeletionFenceWorkspaceAfterLockWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	blockedWS := fx.Workspace(t, "ngd lockwait ws", "ngd-lockwait-"+uuid.NewString()[:8])
	fx.Member(t, blockedWS, fx.UserID, "owner")
	runtimeID, sourceID, agentID, runID := graphSourceFixture(t, fx, blockedWS)
	deletePath := "/api/workspaces/" + blockedWS

	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.Release()
		}
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var blockerPID int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workspace WHERE id=$1 FOR KEY SHARE`, blockedWS); err != nil {
		t.Fatal(err)
	}
	var holderTaskID string
	if err := tx.QueryRow(ctx, `INSERT INTO agent_task_queue
		(agent_id, status, runtime_id, graph_run_id, work_source_id, work_native_id)
		VALUES ($1, 'queued', $2, $3, $4, $5) RETURNING id`,
		agentID, runtimeID, runID, sourceID, "ngd-lock-"+uuid.NewString()).Scan(&holderTaskID); err != nil {
		t.Fatal(err)
	}
	fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE id = $1`, holderTaskID)

	done := asyncSourceReadCall(ctx, http.MethodDelete, deletePath, testToken, blockedWS, "")
	if !waitForBlockedBy(ctx, testPool, blockerPID) {
		t.Fatal("workspace DELETE never blocked behind the held workspace lock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	committed = true
	conn.Release()
	if status := <-done; status != http.StatusConflict {
		t.Fatalf("workspace DELETE after lock wait + committed graph insert: status=%d want=409", status)
	}
	if !rowExists(t, fx, "workspace", blockedWS) || !rowExists(t, fx, "work_source", sourceID) ||
		!rowExists(t, fx, "workflow_run", runID) {
		t.Fatal("post-lock-wait 409 must preserve workspace, source and workflow_run rows")
	}
	if !rowExists(t, fx, "agent_task_queue", holderTaskID) {
		t.Fatal("holder-inserted graph task must survive the rejected workspace delete")
	}
}
