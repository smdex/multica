CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_native_import_identity
    ON chat_session (workspace_id, creator_id, runtime_id, native_import_provider, native_import_id)
    WHERE native_import_id IS NOT NULL;
