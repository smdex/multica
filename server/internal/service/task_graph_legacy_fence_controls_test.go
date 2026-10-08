package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Bounded controls followthrough for the legacy graph fence: a negative graph
// + positive legacy pair for the flow mutators whose matching parameters
// deserve exact-value coverage beyond the shared matrix. Every test proves
// the whole graph row stable (taskJSON) and the matched legacy row mutated
// with the exact values the query was invoked with. Reuses
// graphFenceFixture from task_graph_legacy_fence_test.go.

func TestGraphFenceStartAgentTaskWithRunControls(t *testing.T) {
	f := newGraphFenceFixture(t)
	ctx := context.Background()
	graphID := f.seedGraphTask(t, "dispatched")
	legacyID := f.seedLegacyTask(t, "dispatched")
	before := f.taskJSON(t, graphID)
	runID := util.MustParseUUID("00000000-0000-0000-0000-00000000c0de")

	if _, err := f.q.StartAgentTaskWithRun(ctx, db.StartAgentTaskWithRunParams{ID: graphID, ActiveRunID: runID}); err == nil {
		t.Fatal("StartAgentTaskWithRun started a graph row")
	}
	if after := f.taskJSON(t, graphID); after != before {
		t.Fatalf("graph row mutated:\nbefore %s\nafter  %s", before, after)
	}

	started, err := f.q.StartAgentTaskWithRun(ctx, db.StartAgentTaskWithRunParams{ID: legacyID, ActiveRunID: runID})
	if err != nil {
		t.Fatalf("legacy start-with-run: %v", err)
	}
	if started.Status != "running" || !started.ActiveRunID.Valid || started.ActiveRunID.Bytes != runID.Bytes {
		t.Fatalf("legacy start-with-run = status %q run %v, want running with exact run id", started.Status, started.ActiveRunID)
	}
	if !started.StartedAt.Valid || started.StartedAt.Time.IsZero() {
		t.Fatal("legacy start-with-run did not stamp started_at")
	}
}

func TestGraphFenceStartAgentTaskWithSupplementControls(t *testing.T) {
	f := newGraphFenceFixture(t)
	ctx := context.Background()
	graphID := f.seedGraphTask(t, "dispatched")
	legacyID := f.seedLegacyTask(t, "dispatched")
	before := f.taskJSON(t, graphID)
	runID := util.MustParseUUID("00000000-0000-0000-0000-00000000feed")

	if _, err := f.q.StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{
		TaskID: graphID, ActiveRunID: runID, EnableTaskSupplement: true,
	}); err == nil {
		t.Fatal("StartAgentTaskWithSupplement started a graph row")
	}
	if after := f.taskJSON(t, graphID); after != before {
		t.Fatalf("graph row mutated:\nbefore %s\nafter  %s", before, after)
	}

	// The legacy twin uses the exact same capability handshake inputs and
	// succeeds; supplement capability persistence itself is covered by the
	// supplement suite, so only the task transition + run id are asserted here.
	started, err := f.q.StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{
		TaskID: legacyID, ActiveRunID: runID, EnableTaskSupplement: true,
	})
	if err != nil {
		t.Fatalf("legacy start-with-supplement: %v", err)
	}
	if started.Status != "running" || !started.ActiveRunID.Valid || started.ActiveRunID.Bytes != runID.Bytes {
		t.Fatalf("legacy start-with-supplement = status %q run %v, want running with exact run id", started.Status, started.ActiveRunID)
	}
}

func TestGraphFenceFailAgentTaskExactSettlementControls(t *testing.T) {
	f := newGraphFenceFixture(t)
	ctx := context.Background()
	graphID := f.seedGraphTask(t, "running", testutil.Cols{"started_at": time.Now()})
	legacyID := f.seedLegacyTask(t, "running", testutil.Cols{"started_at": time.Now()})
	before := f.taskJSON(t, graphID)

	graphErr := pgtype.Text{String: "graph must fail through run-scoped recovery", Valid: true}
	failParams := func(id pgtype.UUID) db.FailAgentTaskParams {
		return db.FailAgentTaskParams{
			ID:                    id,
			Error:                 graphErr,
			FailureReason:         graphText("runtime_recovery"),
			SessionRolloutMissing: false,
			SessionID:             graphText("pin-me-not"),
			WorkDir:               graphText("/graph/work"),
			DurableWorkDir:        graphText("/graph/durable"),
			BranchName:            graphText("graph/branch"),
			RetiredSessionID:      graphText("graph-retired"),
		}
	}
	if _, err := f.q.FailAgentTask(ctx, failParams(graphID)); err == nil {
		t.Fatal("FailAgentTask failed a graph row")
	}
	if after := f.taskJSON(t, graphID); after != before {
		t.Fatalf("graph row mutated:\nbefore %s\nafter  %s", before, after)
	}

	failed, err := f.q.FailAgentTask(ctx, failParams(legacyID))
	if err != nil {
		t.Fatalf("legacy fail: %v", err)
	}
	if failed.Status != "failed" ||
		!failed.Error.Valid || failed.Error.String != graphErr.String ||
		!failed.FailureReason.Valid || failed.FailureReason.String != "runtime_recovery" ||
		!failed.SessionID.Valid || failed.SessionID.String != "pin-me-not" ||
		!failed.WorkDir.Valid || failed.WorkDir.String != "/graph/work" ||
		!failed.DurableWorkDir.Valid || failed.DurableWorkDir.String != "/graph/durable" ||
		!failed.BranchName.Valid || failed.BranchName.String != "graph/branch" ||
		!failed.RetiredSessionID.Valid || failed.RetiredSessionID.String != "graph-retired" {
		t.Fatalf("legacy fail settlement mismatch: status=%q err=%v reason=%v session=%v workdir=%v durable=%v branch=%v retired=%v",
			failed.Status, failed.Error, failed.FailureReason, failed.SessionID, failed.WorkDir, failed.DurableWorkDir, failed.BranchName, failed.RetiredSessionID)
	}
	if !failed.CompletedAt.Valid || failed.CompletedAt.Time.IsZero() {
		t.Fatal("legacy fail did not stamp completed_at")
	}
}

func TestGraphFenceReassignTasksToRuntimeControls(t *testing.T) {
	f := newGraphFenceFixture(t)
	ctx := context.Background()
	graphID := f.seedGraphTask(t, "queued")
	legacyID := f.seedLegacyTask(t, "queued")
	before := f.taskJSON(t, graphID)
	newRuntimeID := util.MustParseUUID(f.dbfx.Runtime(t, "graph-fence-reassign-target", testutil.Cols{
		"workspace_id": f.workspaceID,
		"status":       "online",
		"last_seen_at": time.Now(),
	}))

	res, err := f.q.ReassignTasksToRuntime(ctx, db.ReassignTasksToRuntimeParams{
		OldRuntimeID: util.MustParseUUID(f.runtimeID), NewRuntimeID: newRuntimeID,
	})
	if err != nil {
		t.Fatalf("ReassignTasksToRuntime: %v", err)
	}
	if !res.FenceOk || res.ReassignedTasks != 1 {
		t.Fatalf("reassign result = fence %v count %d; graph excluded so exactly the legacy row moves", res.FenceOk, res.ReassignedTasks)
	}
	if after := f.taskJSON(t, graphID); after != before {
		t.Fatalf("graph row mutated:\nbefore %s\nafter  %s", before, after)
	}
	if s := f.taskState(t, legacyID); s.RuntimeID == nil || *s.RuntimeID != util.UUIDToString(newRuntimeID) {
		t.Fatalf("legacy row runtime = %v, want %s", s.RuntimeID, util.UUIDToString(newRuntimeID))
	}
}

func TestGraphFenceFailTasksForOfflineRuntimesControls(t *testing.T) {
	f := newGraphFenceFixture(t)
	ctx := context.Background()
	graphID := f.seedGraphTask(t, "running", testutil.Cols{"started_at": time.Now()})
	legacyID := f.seedLegacyTask(t, "running", testutil.Cols{"started_at": time.Now()})
	before := f.taskJSON(t, graphID)
	if _, err := f.pool.Exec(ctx, `
		UPDATE agent_runtime SET status = 'offline', last_seen_at = now() - interval '24 hours'
		WHERE id = $1`, f.runtimeID); err != nil {
		t.Fatalf("age runtime: %v", err)
	}

	failed, err := f.q.FailTasksForOfflineRuntimes(ctx, db.FailTasksForOfflineRuntimesParams{ReconnectGraceSecs: 0, MaxPerTick: 100})
	if err != nil {
		t.Fatalf("FailTasksForOfflineRuntimes: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != legacyID {
		t.Fatalf("offline fail rows = %d, want exactly the legacy row", len(failed))
	}
	if failed[0].Status != "failed" || !failed[0].FailureReason.Valid || failed[0].FailureReason.String != "runtime_offline" {
		t.Fatalf("legacy offline settlement = %s/%v, want failed/runtime_offline", failed[0].Status, failed[0].FailureReason)
	}
	if after := f.taskJSON(t, graphID); after != before {
		t.Fatalf("graph row mutated:\nbefore %s\nafter  %s", before, after)
	}
}

func TestGraphFenceExpireStaleQueuedTasksControls(t *testing.T) {
	f := newGraphFenceFixture(t)
	ctx := context.Background()
	graphID := f.seedGraphTask(t, "queued", testutil.Cols{"created_at": time.Now().Add(-24 * time.Hour)})
	legacyID := f.seedLegacyTask(t, "queued", testutil.Cols{"created_at": time.Now().Add(-24 * time.Hour)})
	before := f.taskJSON(t, graphID)
	if _, err := f.pool.Exec(ctx, `
		UPDATE agent_runtime SET status = 'offline', last_seen_at = now() - interval '24 hours'
		WHERE id = $1`, f.runtimeID); err != nil {
		t.Fatalf("age runtime: %v", err)
	}

	expired, err := f.q.ExpireStaleQueuedTasks(ctx, db.ExpireStaleQueuedTasksParams{ReconnectGraceSecs: 0, MaxPerTick: 100})
	if err != nil {
		t.Fatalf("ExpireStaleQueuedTasks: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != legacyID {
		t.Fatalf("queued-expiry rows = %d, want exactly the legacy row", len(expired))
	}
	if expired[0].Status != "failed" || !expired[0].FailureReason.Valid || expired[0].FailureReason.String != "queued_expired" {
		t.Fatalf("legacy queued-expiry settlement = %s/%v, want failed/queued_expired", expired[0].Status, expired[0].FailureReason)
	}
	if after := f.taskJSON(t, graphID); after != before {
		t.Fatalf("graph row mutated:\nbefore %s\nafter  %s", before, after)
	}
}
