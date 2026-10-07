CREATE TABLE IF NOT EXISTS work_source (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    project_id UUID,
    runtime_id UUID NOT NULL,
    daemon_id TEXT NOT NULL,
    name TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'observe' CHECK (mode IN ('observe', 'native')),
    enabled BOOLEAN NOT NULL DEFAULT true,
    source_handle TEXT NOT NULL,
    config_revision INTEGER NOT NULL DEFAULT 1,
    last_health TEXT,
    last_error TEXT,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS issue_work_link (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    source_id UUID NOT NULL,
    native_id TEXT NOT NULL,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
