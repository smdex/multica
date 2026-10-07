-- name: CreateWorkSourceCommand :one
INSERT INTO work_source_command (
    id, workspace_id, source_id, command, native_id, limit_count,
    status, request_hash, created_by, request_id, config_revision
)
VALUES (
    @id, @workspace_id, @source_id, @command, sqlc.narg(native_id), sqlc.narg(limit_count),
    'pending', @request_hash, sqlc.narg(created_by), @request_id, @config_revision
)
RETURNING *;

-- name: GetWorkSourceCommandInFlightForUpdate :one
-- Locks the source's single in-flight command row (enforced by
-- work_source_command_inflight_idx) for the create/claim race windows.
-- Must run inside the caller's transaction after LockWorkSourceForWrite.
SELECT * FROM work_source_command
WHERE source_id = $1 AND workspace_id = $2
  AND status IN ('pending', 'claimed')
FOR UPDATE;

-- name: GetWorkSourceCommandInWorkspace :one
SELECT * FROM work_source_command
WHERE id = $1 AND workspace_id = $2;

-- name: GetWorkSourceCommandForUpdate :one
SELECT * FROM work_source_command WHERE id = $1 AND workspace_id = $2 FOR UPDATE;

-- name: GetWorkSourceCommandRuntimeForShare :one
SELECT * FROM agent_runtime WHERE id = $1 FOR SHARE;

-- name: GetWorkSourceCommandByRequest :one
SELECT * FROM work_source_command
WHERE source_id = @source_id AND workspace_id = @workspace_id AND request_id = @request_id
FOR UPDATE;

-- name: ExpireWorkSourceCommandsForSource :execrows
UPDATE work_source_command SET status = 'failed', result = NULL, claimed_runtime_id = NULL,
error = 'Read command expired before completion', updated_at = now()
WHERE source_id = $1 AND workspace_id = $2 AND status IN ('pending', 'claimed') AND expires_at <= statement_timestamp();

-- name: ListExpiredWorkSourceCommandSources :many
-- Root sweeper locks each source before calling ExpireWorkSourceCommandsForSource.
SELECT source_id, workspace_id FROM work_source_command
WHERE status IN ('pending', 'claimed') AND expires_at <= statement_timestamp()
ORDER BY expires_at, id LIMIT $1;

-- name: ClaimWorkSourceCommand :one
-- CAS pending -> claimed by exactly one runtime; a stale or concurrent
-- claim attempt reads 0 rows and the caller reports not-claimable.
UPDATE work_source_command
SET status = 'claimed',
    claimed_runtime_id = @runtime_id,
    claimed_at = now(),
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND status = 'pending'
  AND expires_at > clock_timestamp()
RETURNING *;

-- name: CompleteWorkSourceCommand :one
-- Terminal CAS claimed -> succeeded; only the claiming runtime's report
-- transitions the receipt and stores the bounded result.
UPDATE work_source_command
SET status = 'succeeded',
    result = @result,
    error = NULL,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND status = 'claimed'
  AND claimed_runtime_id = @runtime_id
  AND expires_at > clock_timestamp()
RETURNING *;

-- name: FailWorkSourceCommand :one
-- Terminal CAS claimed -> failed with a bounded diagnostic; only the
-- claiming runtime can record it.
UPDATE work_source_command
SET status = 'failed',
    error = @error,
    result = NULL,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND status = 'claimed'
  AND claimed_runtime_id = @runtime_id
  AND expires_at > clock_timestamp()
RETURNING *;

-- name: ListWorkSourceCommandsBySource :many
-- Bounded receipt history; the service caps limit_count at 200.
SELECT id, workspace_id, source_id, request_id, config_revision, expires_at,
command, native_id, limit_count, status, claimed_runtime_id, claimed_at,
request_hash, NULL::text AS result,
error, created_by, created_at, updated_at FROM work_source_command
WHERE source_id = $1 AND workspace_id = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: DeleteWorkSourceCommandsForWorkSource :execrows
-- Source cascade teardown helper (application-code cleanup, no FKs by
-- project rule); called by the source delete sweep.
DELETE FROM work_source_command
WHERE source_id = $1 AND workspace_id = $2;

-- name: DeleteWorkSourceCommandsByWorkspace :execrows
-- Workspace teardown helper.
DELETE FROM work_source_command
WHERE workspace_id = $1;
