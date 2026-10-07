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
