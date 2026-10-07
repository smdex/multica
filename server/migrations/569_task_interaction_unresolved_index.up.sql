CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_task_interaction_unresolved
    ON task_interaction (chat_session_id, expires_at)
    WHERE status IN ('pending', 'resolving', 'unknown');
