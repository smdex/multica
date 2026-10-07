DROP TABLE IF EXISTS task_interaction;
DROP TABLE IF EXISTS agent_workflow_request;

ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS control_updated_at,
    DROP COLUMN IF EXISTS control_state,
    DROP COLUMN IF EXISTS active_run_id,
    DROP COLUMN IF EXISTS resume_policy,
    DROP COLUMN IF EXISTS interaction_mode;

ALTER TABLE chat_message
    DROP COLUMN IF EXISTS input_request_id,
    DROP COLUMN IF EXISTS native_message_id,
    DROP COLUMN IF EXISTS imported_events;

ALTER TABLE chat_session
    DROP COLUMN IF EXISTS native_imported_at,
    DROP COLUMN IF EXISTS native_import_revision,
    DROP COLUMN IF EXISTS native_import_id,
    DROP COLUMN IF EXISTS native_import_provider,
    DROP COLUMN IF EXISTS interaction_mode;

ALTER TABLE agent_runtime
    DROP COLUMN IF EXISTS agent_workflow_capabilities;
