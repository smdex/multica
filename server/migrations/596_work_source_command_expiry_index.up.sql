CREATE INDEX CONCURRENTLY IF NOT EXISTS work_source_command_expiry_idx
    ON work_source_command (expires_at, id)
    WHERE status IN ('pending', 'claimed');
