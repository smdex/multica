CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_work_link_relationship
    ON issue_work_link (source_id, native_id, issue_id);
