CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS work_source_command_request_idx ON work_source_command (workspace_id, source_id, request_id);
