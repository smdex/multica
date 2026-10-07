CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_workflow_request_native_import_active
    ON agent_workflow_request (workspace_id, requester_id, runtime_id, native_source_id)
    WHERE kind = 'native_session_import'
      AND status IN ('pending', 'running', 'unknown')
      AND native_source_id IS NOT NULL;
