ALTER TABLE task_message
    ADD COLUMN batch_id uuid,
    ADD COLUMN batch_hash text;
