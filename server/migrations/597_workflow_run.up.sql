-- Frozen observation sets only. No queue admission or source write authority.
CREATE TABLE IF NOT EXISTS workflow_run (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    project_id UUID,
    source_id UUID NOT NULL,
    request_id UUID NOT NULL,
    request_hash TEXT NOT NULL,
    root_native_id TEXT NOT NULL,
    config_revision INTEGER NOT NULL,
    capacity INTEGER NOT NULL CHECK (capacity BETWEEN 1 AND 2),
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status = 'draft'),
    graph JSONB NOT NULL,
    node_state JSONB NOT NULL,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
