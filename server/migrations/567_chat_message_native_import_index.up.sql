CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_native_import_identity
    ON chat_message (chat_session_id, native_message_id)
    WHERE native_message_id IS NOT NULL;
