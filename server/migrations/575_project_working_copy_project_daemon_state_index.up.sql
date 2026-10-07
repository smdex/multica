CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_project_working_copy_project_daemon_state
    ON project_working_copy (workspace_id, project_id, daemon_id, state);
