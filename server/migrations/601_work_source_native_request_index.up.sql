CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS work_source_native_request_idx ON work_source (workspace_id, native_request_id) WHERE native_request_id IS NOT NULL;
