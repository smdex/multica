CREATE TABLE project_working_copy (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    project_id UUID NOT NULL,
    daemon_id TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('directory', 'git_worktree')),
    canonical_path TEXT,
    ownership TEXT NOT NULL CHECK (ownership IN ('external', 'managed')),
    source_resource_id UUID,
    state TEXT NOT NULL DEFAULT 'provisioning'
        CHECK (state IN ('provisioning', 'ready', 'failed', 'unavailable', 'archived')),
    provisioning_request_id UUID NOT NULL,
    provisioning_spec JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_error TEXT,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    delete_requested_at TIMESTAMPTZ
);
