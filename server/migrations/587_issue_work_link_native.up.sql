-- One native work item links to at most one issue per source binding.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_work_link_native
    ON issue_work_link (source_id, native_id);
