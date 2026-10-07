ALTER TABLE chat_session
    ADD COLUMN IF NOT EXISTS working_copy_id UUID;
