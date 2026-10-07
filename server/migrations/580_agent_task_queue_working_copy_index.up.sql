CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_working_copy
    ON agent_task_queue (working_copy_id)
    WHERE working_copy_id IS NOT NULL;
