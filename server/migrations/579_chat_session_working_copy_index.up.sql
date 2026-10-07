CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_working_copy
    ON chat_session (working_copy_id)
    WHERE working_copy_id IS NOT NULL;
