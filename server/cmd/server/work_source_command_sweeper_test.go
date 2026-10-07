package main

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestExpiredWorkSourceSelectionSkipsOrphans(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	orphanSource := uuid.NewString()
	fx.Insert(t, "work_source_command", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"source_id": orphanSource, "request_id": uuid.NewString(),
		"config_revision": 1, "command": "list", "status": "pending",
		"request_hash": "orphan-expiry-regression", "expires_at": testutil.Raw("'-infinity'::timestamptz"),
	})
	rows, err := db.New(testPool).ListExpiredWorkSourceCommandSources(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if fx.Count(t, `SELECT count(*) FROM work_source WHERE id=$1 AND workspace_id=$2`, row.SourceID, row.WorkspaceID) != 1 {
			t.Fatal("orphaned expired receipt consumed bounded sweeper selection")
		}
	}
}

func TestSweepExpiredWorkSourceCommands(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	runtimeID := createRuntimeGCFixtureRuntime(t, ctx, "source-command-expiry")
	fixtures := testutil.New(testPool, testWorkspaceID, testUserID)
	ids := make(map[string]string)
	for _, state := range []string{"pending", "claimed", "fresh", "succeeded"} {
		sourceID := fixtures.Insert(t, "work_source", testutil.Cols{
			"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
			"runtime_id": runtimeID, "daemon_id": "source-command-expiry", "name": state,
			"source_handle": state, "mode": "observe",
		})
		status := state
		deadline := testutil.Raw("now() - interval '1 minute'")
		if state == "fresh" {
			status = "pending"
			deadline = testutil.Raw("now() + interval '1 hour'")
		}
		cols := testutil.Cols{
			"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
			"source_id": sourceID, "request_id": testutil.Raw("gen_random_uuid()"),
			"config_revision": 1, "command": "list", "status": status,
			"request_hash": state, "expires_at": deadline,
		}
		if state == "claimed" {
			cols["claimed_runtime_id"] = runtimeID
		}
		if state == "succeeded" {
			cols["result"] = "[]"
		}
		ids[state] = fixtures.Insert(t, "work_source_command", cols)
	}
	commands := service.NewWorkSourceCommandService(db.New(testPool), testPool)
	sweepExpiredWorkSourceCommands(ctx, commands)
	sweepExpiredWorkSourceCommands(ctx, commands)

	for _, state := range []string{"pending", "claimed"} {
		count := fixtures.Count(t, `SELECT count(*) FROM work_source_command
			WHERE id = $1 AND status = 'failed' AND error <> '' AND result IS NULL AND claimed_runtime_id IS NULL`, ids[state])
		if count != 1 {
			t.Fatalf("expired %s command was not terminally fenced: count=%d", state, count)
		}
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source_command WHERE id = $1 AND status = 'pending'`, ids["fresh"]); count != 1 {
		t.Fatalf("fresh read command was expired: count=%d", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source_command WHERE id = $1 AND status = 'succeeded' AND result = '[]'`, ids["succeeded"]); count != 1 {
		t.Fatalf("terminal success receipt was changed: count=%d", count)
	}
}
