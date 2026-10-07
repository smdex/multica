-- name: ListProjectWorkingCopies :many
SELECT * FROM project_working_copy
WHERE workspace_id = $1 AND project_id = $2
ORDER BY created_at ASC, id ASC;

-- name: GetProjectWorkingCopyInWorkspace :one
SELECT * FROM project_working_copy
WHERE id = $1 AND workspace_id = $2;

-- name: GetProjectWorkingCopyForProjectInWorkspace :one
SELECT * FROM project_working_copy
WHERE id = $1 AND workspace_id = $2 AND project_id = $3;

-- name: GetProjectWorkingCopyByProvisioningRequest :one
SELECT * FROM project_working_copy
WHERE provisioning_request_id = $1;

-- name: CreateProjectWorkingCopy :one
INSERT INTO project_working_copy (
    id, workspace_id, project_id, daemon_id, name, kind, ownership,
    source_resource_id, provisioning_request_id, provisioning_spec, created_by
) VALUES (
    @id, @workspace_id, @project_id, @daemon_id, @name, @kind, @ownership,
    sqlc.narg(source_resource_id), @provisioning_request_id, @provisioning_spec, sqlc.narg(created_by)
)
RETURNING *;

-- name: ListProjectWorkingCopyReconciliationsForDaemon :many
SELECT * FROM project_working_copy
WHERE workspace_id = $1
  AND daemon_id = $2
  AND (state = 'provisioning' OR (state = 'archived' AND delete_requested_at IS NOT NULL))
ORDER BY created_at ASC, id ASC;

-- name: GetReadyProjectWorkingCopyForChatBindingForUpdate :one
SELECT * FROM project_working_copy
WHERE id = $1
  AND workspace_id = $2
  AND project_id = $3
  AND daemon_id = $4
  AND state = 'ready'
  AND canonical_path IS NOT NULL
FOR UPDATE;

-- name: GetReadyProjectWorkingCopyForClaim :one
SELECT wc.*
FROM project_working_copy AS wc
JOIN chat_session AS cs ON cs.id = $2
WHERE wc.id = $1
  AND wc.workspace_id = $3
  AND wc.daemon_id = $4
  AND wc.state = 'ready'
  AND wc.canonical_path IS NOT NULL
  AND cs.workspace_id = wc.workspace_id
  AND cs.project_id = wc.project_id
  AND cs.working_copy_id = wc.id;

-- name: MarkProjectWorkingCopyReady :one
UPDATE project_working_copy
SET canonical_path = @canonical_path,
    state = 'ready',
    last_error = NULL,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND daemon_id = @daemon_id
  AND provisioning_request_id = @provisioning_request_id
  AND state = 'provisioning'
RETURNING *;

-- name: MarkProjectWorkingCopyFailed :one
UPDATE project_working_copy
SET state = 'failed',
    last_error = @last_error,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND daemon_id = @daemon_id
  AND provisioning_request_id = @provisioning_request_id
  AND state = 'provisioning'
RETURNING *;

-- name: ArchiveProjectWorkingCopy :one
UPDATE project_working_copy
SET state = 'archived',
    archived_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND project_id = $3
  AND state <> 'archived'
RETURNING *;

-- name: RequestManagedProjectWorkingCopyDelete :one
UPDATE project_working_copy
SET state = 'archived',
    archived_at = COALESCE(archived_at, now()),
    delete_requested_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND project_id = $3
  AND ownership = 'managed'
RETURNING *;

-- name: DeleteExternalProjectWorkingCopy :exec
DELETE FROM project_working_copy
WHERE id = $1 AND workspace_id = $2 AND project_id = $3
  AND ownership = 'external';

-- name: DeleteManagedProjectWorkingCopyAfterCleanup :exec
DELETE FROM project_working_copy
WHERE id = $1 AND workspace_id = $2 AND daemon_id = $3
  AND ownership = 'managed'
  AND state = 'archived'
  AND delete_requested_at IS NOT NULL;
