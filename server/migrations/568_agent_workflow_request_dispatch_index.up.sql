CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_workflow_request_dispatch
    ON agent_workflow_request (runtime_id, created_at)
    WHERE status = 'pending';
