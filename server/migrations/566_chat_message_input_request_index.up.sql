CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_input_request_id
    ON chat_message (input_request_id)
    WHERE input_request_id IS NOT NULL;
