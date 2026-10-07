-- One in-flight (pending or claimed) command per source. Stable request_id
-- retries return their existing receipt after request_hash verification,
-- including terminal receipts. Independent requests conflict while occupied.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS work_source_command_inflight_idx
    ON work_source_command (source_id)
    WHERE status IN ('pending', 'claimed');
