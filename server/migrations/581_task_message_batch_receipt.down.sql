ALTER TABLE task_message
    DROP COLUMN IF EXISTS batch_hash,
    DROP COLUMN IF EXISTS batch_id;
