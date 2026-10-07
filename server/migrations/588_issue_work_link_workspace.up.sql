CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_work_link_workspace
    ON issue_work_link (workspace_id);
