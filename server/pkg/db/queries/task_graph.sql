-- name: CreateDraftWorkflowRun :one
INSERT INTO workflow_run (
    id, workspace_id, project_id, source_id, request_id, request_hash,
    root_native_id, config_revision, capacity, graph, node_state, created_by
) VALUES (
    @id, @workspace_id, sqlc.narg(project_id), @source_id, @request_id, @request_hash,
    @root_native_id, @config_revision, @capacity, @graph, @node_state, @created_by
)
RETURNING *;

-- name: GetWorkflowRunInWorkspace :one
SELECT * FROM workflow_run WHERE id = $1 AND workspace_id = $2;

-- name: GetWorkflowRunByRequest :one
SELECT * FROM workflow_run WHERE workspace_id = $1 AND request_id = $2;

-- name: DeleteWorkflowRunsForSource :exec
DELETE FROM workflow_run WHERE source_id = $1 AND workspace_id = $2;

-- name: DeleteWorkflowRunsByWorkspace :exec
DELETE FROM workflow_run WHERE workspace_id = $1;

-- name: ClearWorkflowRunProject :exec
UPDATE workflow_run SET project_id = NULL WHERE project_id = $1 AND workspace_id = $2;

-- name: LockDraftWorkflowRequest :exec
-- A durable request UUID is workspace-scoped, including across source changes.
SELECT pg_advisory_xact_lock(hashtext((sqlc.arg(workspace_id)::uuid)::text),
                            hashtext('workflow-draft:' || (sqlc.arg(request_id)::uuid)::text));

-- name: ExistsGraphReservationForSource :one
-- Conservative deletion fence: ANY graph-reserved queue row blocks the
-- source cascade. Both arms are workspace-scoped: exact work_source_id
-- resolved through work_source, or graph_run_id referencing a workflow_run
-- this sweep would delete. Terminal-certain rows also block: no
-- graph-aware retention decision exists yet.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue q
    WHERE q.graph_run_id IS NOT NULL
      AND (q.work_source_id IN (SELECT ws.id FROM work_source ws
                                WHERE ws.id = $1 AND ws.workspace_id = $2)
           OR q.graph_run_id IN (SELECT wr.id FROM workflow_run wr
                                 WHERE wr.source_id = $1 AND wr.workspace_id = $2))
);

-- name: ExistsGraphReservationForWorkspace :one
-- Conservative deletion fence: ANY graph-reserved queue row blocks the
-- workspace teardown, matched through each sweep ownership path (source,
-- agent, runtime) OR by graph_run_id referencing a workflow_run the sweep
-- deletes. OR, not conjunction, so a cross-owned row cannot escape the
-- actual delete sweep. Terminal-certain rows also block.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue t
    WHERE t.graph_run_id IS NOT NULL
      AND (t.work_source_id IN (SELECT ws.id FROM work_source ws WHERE ws.workspace_id = $1)
           OR t.agent_id IN (SELECT a.id FROM agent a WHERE a.workspace_id = $1)
           OR t.runtime_id IN (SELECT ar.id FROM agent_runtime ar WHERE ar.workspace_id = $1)
           OR t.graph_run_id IN (SELECT wr.id FROM workflow_run wr WHERE wr.workspace_id = $1))
);
