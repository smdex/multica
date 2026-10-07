-- name: CreateWorkSource :one
INSERT INTO work_source (
    id, workspace_id, project_id, runtime_id, daemon_id,
    name, mode, enabled, source_handle, created_by
)
VALUES (
    @id, @workspace_id, sqlc.narg(project_id), @runtime_id, @daemon_id,
    @name, @mode, @enabled, @source_handle, sqlc.narg(created_by)
)
RETURNING *;

-- name: LockWorkSourceForWrite :one
-- Serializes source-scoped writes (link insert, cascade delete) against the
-- same row so a link cannot commit after the delete transaction has swept
-- existing links. FOR UPDATE matches LockTaskForMessageBatch's pattern;
-- link-insert takes this lock before inserting, delete takes it before
-- sweeping links, and both are inside their full validation transaction.
SELECT id FROM work_source
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: GetWorkSourceOwnerConflict :one
-- Detects the same physical source (daemon + handle) already registered,
-- including under a different runtime profile, before insert.
SELECT * FROM work_source
WHERE workspace_id = $1 AND daemon_id = $2 AND source_handle = $3;

-- name: ListWorkSourcesByWorkspace :many
SELECT * FROM work_source
WHERE workspace_id = $1
ORDER BY created_at;

-- name: GetWorkSourceInWorkspace :one
SELECT * FROM work_source
WHERE id = $1 AND workspace_id = $2;

-- name: UpdateWorkSourceConfig :one
-- Rename/enable updates only: identity (id, workspace, project,
-- runtime/daemon owner), mode, and source_handle stay immutable per the
-- scoped-source contract (the unique owner key is (workspace_id, daemon_id,
-- source_handle)); the stable source UUID survives name/config changes.
-- enabled is COALESCE'd so an omitted flag keeps the stored value: a rename
-- must never silently re-enable a disabled source.
UPDATE work_source
SET name = @name,
    enabled = COALESCE(sqlc.narg(enabled), enabled),
    config_revision = config_revision + 1,
    updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id
  AND NOT (COALESCE(sqlc.narg(enabled), enabled) AND mode = 'native' AND native_enrolled_at IS NULL)
RETURNING *;

-- name: LockNativeSourceRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended('native-source-request:' || @request_key::text, 0));

-- name: GetNativeSourceByRequest :one
SELECT * FROM work_source
WHERE workspace_id = @workspace_id AND native_request_id = @native_request_id
FOR UPDATE;

-- name: CreateNativeSourceIntent :one
INSERT INTO work_source (
    id, workspace_id, runtime_id, daemon_id, name, mode, enabled,
    source_handle, created_by, native_request_id, native_request_hash,
    native_enrollment_id, native_owner_member_id, native_runtime_created_at
)
VALUES (
    @id, @workspace_id, @runtime_id, @daemon_id, @name, 'native', FALSE,
    @source_handle, @created_by, @native_request_id, @native_request_hash,
    @native_enrollment_id, @native_owner_member_id, @native_runtime_created_at
)
RETURNING *;

-- name: ApproveNativeSourceEnrollment :one
UPDATE work_source
SET native_manifest_hash = @native_manifest_hash,
    native_approved_at = COALESCE(native_approved_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE id = @id AND workspace_id = @workspace_id AND mode = 'native'
  AND native_enrollment_id = @native_enrollment_id
  AND config_revision = @config_revision
  AND (native_manifest_hash IS NULL OR native_manifest_hash = @native_manifest_hash)
RETURNING *;

-- name: FinalizeNativeSourceEnrollment :one
UPDATE work_source
SET native_enrolled_at = COALESCE(native_enrolled_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE id = @id AND workspace_id = @workspace_id AND mode = 'native'
  AND native_enrollment_id = @native_enrollment_id
  AND config_revision = @config_revision
  AND native_manifest_hash = @native_manifest_hash
  AND native_approved_at IS NOT NULL
RETURNING *;

-- name: UpdateWorkSourceHealth :exec
UPDATE work_source
SET last_health = @last_health,
    last_error = sqlc.narg(last_error),
    updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id;

-- name: DeleteWorkSource :execrows
DELETE FROM work_source
WHERE id = $1 AND workspace_id = $2;

-- name: CreateIssueWorkLink :one
INSERT INTO issue_work_link (
    id, workspace_id, issue_id, source_id, native_id, created_by
)
VALUES (
    @id, @workspace_id, @issue_id, @source_id, @native_id, sqlc.narg(created_by)
)
RETURNING *;

-- name: ListIssueWorkLinksByIssue :many
SELECT * FROM issue_work_link
WHERE issue_id = $1 AND workspace_id = $2
ORDER BY created_at;

-- name: ListIssueWorkLinksBySource :many
SELECT * FROM issue_work_link
WHERE source_id = $1 AND workspace_id = $2
ORDER BY created_at;

-- name: GetIssueWorkLinkInWorkspace :one
SELECT * FROM issue_work_link
WHERE id = $1 AND workspace_id = $2;

-- name: DeleteIssueWorkLink :execrows
-- Unlink has no side effect on the issue or the source-owned work item.
DELETE FROM issue_work_link
WHERE id = $1 AND workspace_id = $2;

-- name: DeleteWorkSourcesByWorkspace :execrows
-- Workspace deletion teardown: links first (DeleteIssueWorkLinksByWorkspace),
-- then sources. Application-code cleanup per the no-FK rule.
DELETE FROM work_source
WHERE workspace_id = $1;

-- name: DeleteIssueWorkLinksByWorkspace :execrows
DELETE FROM issue_work_link
WHERE workspace_id = $1;

-- name: DeleteIssueWorkLinksForIssue :execrows
DELETE FROM issue_work_link
WHERE issue_id = $1 AND workspace_id = $2;

-- name: DeleteIssueWorkLinksForWorkSource :execrows
DELETE FROM issue_work_link
WHERE source_id = $1 AND workspace_id = $2;

-- name: ClearWorkSourceProject :exec
-- Project deletion retains the workspace source and native task links.
UPDATE work_source
SET project_id = NULL, config_revision = config_revision + 1, updated_at = now()
WHERE project_id = $1 AND workspace_id = $2;
