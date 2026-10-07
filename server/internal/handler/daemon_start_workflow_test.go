package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func workflowStartRequest(f supplementFixture, generation time.Time, runID string, claimed, supplement bool) *http.Request {
	body := map[string]any{"run_id": runID}
	if claimed {
		body["runtime_id"] = f.runtimeID
		body["dispatched_at"] = generation.Format(time.RFC3339Nano)
	}
	if supplement {
		body["capabilities"] = []string{protocol.DaemonCapabilityTaskSupplementV1}
	}
	return withURLParam(newDaemonTokenRequest(http.MethodPost, "/start", body, testWorkspaceID, "workflow-start-test"), "taskId", f.taskID)
}

func TestStartClaimWorkflowRunPreservesSupplementsAndReplay(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		for _, state := range []string{"dispatched", "waiting_local_directory"} {
			t.Run(fmt.Sprintf("claimed=%t/%s", claimed, state), func(t *testing.T) {
				f := newSupplementFixture(t, "codex", state, false)
				dbfx.Cleanup(t, `DELETE FROM task_supplement_capability WHERE task_id=$1`, f.taskID)
				generation := time.Date(2026, 9, 23, 0, 0, 0, 123456000, time.UTC)
				runID := uuid.NewString()
				dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at=$2, active_run_id=$3,
					control_state='{"active":true}'::jsonb, control_updated_at=now() WHERE id=$1`, f.taskID, generation, uuid.NewString())

				var response AgentTaskResponse
				testutil.Call(t, testHandler.StartTask, workflowStartRequest(f, generation, runID, claimed, true)).Want(http.StatusOK).JSON(&response)
				if response.SupplementCapability != protocol.DaemonCapabilityTaskSupplementV1 {
					t.Fatalf("run start lost negotiated supplements: %+v", response)
				}
				started, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
				if err != nil {
					t.Fatal(err)
				}
				if started.Status != "running" || started.ActiveRunID != parseUUID(runID) || !started.StartedAt.Valid || len(started.ControlState) != 0 || started.ControlUpdatedAt.Valid {
					t.Fatalf("run and cleared controls did not commit with start: %+v", started)
				}

				dbfx.Exec(t, `UPDATE agent_task_queue SET control_state='{"active":true,"turn_id":"live"}'::jsonb,
					control_updated_at=now() WHERE id=$1`, f.taskID)
				beforeReplay, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
				if err != nil {
					t.Fatal(err)
				}
				if claimed {
					// A replay uses the committed handshake even if the repeated offer changes.
					testutil.Call(t, testHandler.StartTask, workflowStartRequest(f, generation, runID, true, false)).Want(http.StatusOK).JSON(&response)
					if response.SupplementCapability != protocol.DaemonCapabilityTaskSupplementV1 {
						t.Fatalf("run replay renegotiated supplements: %+v", response)
					}
					testutil.Call(t, testHandler.StartTask, workflowStartRequest(f, generation, uuid.NewString(), true, true)).Want(http.StatusConflict)
					testutil.Call(t, testHandler.StartTask, workflowStartRequest(f, generation.Add(-time.Microsecond), runID, true, true)).Want(http.StatusConflict)
				} else {
					testutil.Call(t, testHandler.StartTask, workflowStartRequest(f, generation, runID, false, true)).Want(http.StatusBadRequest)
				}
				afterReplay, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
				if err != nil {
					t.Fatal(err)
				}
				if afterReplay.ActiveRunID != started.ActiveRunID || !afterReplay.StartedAt.Time.Equal(started.StartedAt.Time) || !bytes.Equal(afterReplay.ControlState, beforeReplay.ControlState) || !afterReplay.ControlUpdatedAt.Time.Equal(beforeReplay.ControlUpdatedAt.Time) {
					t.Fatalf("replay changed execution identity, start time, or live controls: before=%+v after=%+v", beforeReplay, afterReplay)
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id=$1`, f.taskID); n != 1 {
					t.Fatalf("run start/replay wrote %d capability rows", n)
				}
			})
		}
	}
}

func TestStartClaimWorkflowReconciliationRollsBackRunAndSupplements(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprintf("claimed=%t", claimed), func(t *testing.T) {
			f := newSupplementFixture(t, "codex", "dispatched", false)
			dbfx.Cleanup(t, `DELETE FROM task_supplement_capability WHERE task_id=$1`, f.taskID)
			generation := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
			oldRun, runID := uuid.NewString(), uuid.NewString()
			dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at=$2, active_run_id=$3 WHERE id=$1`, f.taskID, generation, oldRun)
			// Fail after the supplemental start statement, during workflow reconciliation.
			// A statement trigger also covers the empty-dependent case without inventing an interaction.
			dbfx.Exec(t, fmt.Sprintf(`
				CREATE FUNCTION reject_workflow_start_reconciliation() RETURNS trigger AS $$
				BEGIN
					IF EXISTS (SELECT 1 FROM agent_task_queue WHERE id=TG_ARGV[0]::uuid AND active_run_id=TG_ARGV[1]::uuid) THEN
						RAISE EXCEPTION 'workflow reconciliation rejected';
					END IF;
					RETURN NULL;
				END;
				$$ LANGUAGE plpgsql;
				CREATE TRIGGER reject_workflow_start_reconciliation BEFORE UPDATE ON task_interaction
				FOR EACH STATEMENT EXECUTE FUNCTION reject_workflow_start_reconciliation('%s', '%s')
			`, f.taskID, runID))
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_workflow_start_reconciliation ON task_interaction`)
				_, _ = testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS reject_workflow_start_reconciliation()`)
			})

			testutil.Call(t, testHandler.StartTask, workflowStartRequest(f, generation, runID, claimed, true)).Want(http.StatusInternalServerError)
			task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
			if err != nil {
				t.Fatal(err)
			}
			if task.Status != "dispatched" || task.ActiveRunID != parseUUID(oldRun) || task.StartedAt.Valid {
				t.Fatalf("failed reconciliation partially committed run start: %+v", task)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id=$1`, f.taskID); n != 0 {
				t.Fatalf("failed reconciliation committed %d supplement capabilities", n)
			}
		})
	}
}
