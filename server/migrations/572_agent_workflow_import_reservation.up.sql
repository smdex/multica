ALTER TABLE agent_workflow_request
    ADD COLUMN IF NOT EXISTS native_source_id TEXT;
