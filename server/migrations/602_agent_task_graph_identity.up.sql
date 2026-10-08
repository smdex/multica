-- Graph attempts are an additive queue kind, never quick-create/chat/Issue rows.
-- Legacy writers omit these columns. Application transactions validate the
-- run/source relationships and exact claim/incarnation fences, without FKs.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS graph_run_id UUID,
    ADD COLUMN IF NOT EXISTS work_source_id UUID,
    ADD COLUMN IF NOT EXISTS work_native_id TEXT,
    ADD COLUMN IF NOT EXISTS execution_uncertain BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE agent_task_queue
    ADD CONSTRAINT agent_task_graph_identity_check CHECK (
        (graph_run_id IS NULL AND work_source_id IS NULL AND work_native_id IS NULL
            AND NOT execution_uncertain)
        OR
        (graph_run_id IS NOT NULL AND work_source_id IS NOT NULL
            AND work_native_id IS NOT NULL AND length(btrim(work_native_id)) > 0
            AND graph_run_id <> '00000000-0000-0000-0000-000000000000'::uuid
            AND work_source_id <> '00000000-0000-0000-0000-000000000000'::uuid
            AND agent_id IS NOT NULL AND runtime_id IS NOT NULL
            AND issue_id IS NULL AND chat_session_id IS NULL
            AND autopilot_run_id IS NULL AND context->>'wakeup_id' IS NULL)
    );
