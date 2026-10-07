-- Physical source identity: the same daemon + approved source handle cannot
-- be registered twice as different execution owners, even across runtime
-- profiles on the same daemon.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_work_source_owner_identity
    ON work_source (workspace_id, daemon_id, source_handle);
