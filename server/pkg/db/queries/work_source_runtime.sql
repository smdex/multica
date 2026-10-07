-- name: RuntimeHasWorkSources :one
-- Runtime deletion/GC must not discard an approved source owner binding.
SELECT EXISTS (
    SELECT 1 FROM work_source
    WHERE runtime_id = $1 AND workspace_id = $2
);
