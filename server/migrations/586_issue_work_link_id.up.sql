CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_work_link_id
    ON issue_work_link (id);
