-- Rollback refuses when a native item now links to multiple Issues. Resolve
-- those relationships explicitly first; never delete links automatically.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_work_link_native
    ON issue_work_link (source_id, native_id);
