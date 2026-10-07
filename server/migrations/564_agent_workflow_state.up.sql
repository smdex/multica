ALTER TABLE agent_runtime
    ADD COLUMN IF NOT EXISTS agent_workflow_capabilities JSONB;

ALTER TABLE chat_session
    ADD COLUMN IF NOT EXISTS interaction_mode TEXT NOT NULL DEFAULT 'autonomous',
    ADD COLUMN IF NOT EXISTS native_import_provider TEXT,
    ADD COLUMN IF NOT EXISTS native_import_id TEXT,
    ADD COLUMN IF NOT EXISTS native_import_revision TEXT,
    ADD COLUMN IF NOT EXISTS native_imported_at TIMESTAMPTZ;

ALTER TABLE chat_message
    ADD COLUMN IF NOT EXISTS imported_events JSONB,
    ADD COLUMN IF NOT EXISTS native_message_id TEXT,
    ADD COLUMN IF NOT EXISTS input_request_id UUID;

ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS interaction_mode TEXT NOT NULL DEFAULT 'autonomous',
    ADD COLUMN IF NOT EXISTS resume_policy TEXT NOT NULL DEFAULT 'allow_fresh',
    ADD COLUMN IF NOT EXISTS active_run_id UUID,
    ADD COLUMN IF NOT EXISTS control_state JSONB,
    ADD COLUMN IF NOT EXISTS control_updated_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS agent_workflow_request (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    requester_id UUID NOT NULL,
    runtime_id UUID NOT NULL,
    chat_session_id UUID,
    task_id UUID,
    run_id UUID,
    turn_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('native_session_list', 'native_session_import', 'steer', 'interaction_response')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'failed', 'unknown')),
    request_hash TEXT NOT NULL,
    request JSONB NOT NULL,
    result JSONB,
    error JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    dispatched_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS task_interaction (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    runtime_id UUID NOT NULL,
    chat_session_id UUID NOT NULL,
    task_id UUID NOT NULL,
    run_id UUID NOT NULL,
    turn_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('approval', 'question')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'resolving', 'resolved', 'expired', 'cancelled', 'unknown')),
    version BIGINT NOT NULL DEFAULT 1,
    request JSONB NOT NULL,
    response_request_id UUID,
    response JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
