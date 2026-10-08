ALTER TABLE agent_task_queue
    DROP CONSTRAINT IF EXISTS agent_task_graph_identity_check,
    DROP COLUMN IF EXISTS execution_uncertain,
    DROP COLUMN IF EXISTS work_native_id,
    DROP COLUMN IF EXISTS work_source_id,
    DROP COLUMN IF EXISTS graph_run_id;
