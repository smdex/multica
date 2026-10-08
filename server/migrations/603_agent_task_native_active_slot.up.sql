-- A failed legacy status cannot release a possibly-live graph execution slot.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS agent_task_native_active_slot
    ON agent_task_queue (work_source_id, work_native_id)
    WHERE graph_run_id IS NOT NULL
      AND (status IN ('dispatched', 'running', 'waiting_local_directory')
           OR execution_uncertain);
