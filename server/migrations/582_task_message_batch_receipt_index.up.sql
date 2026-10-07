CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_message_batch_receipt_idx
    ON task_message (task_id, batch_id, seq)
    WHERE batch_id IS NOT NULL;
