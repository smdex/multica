ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS working_copy_id UUID;
