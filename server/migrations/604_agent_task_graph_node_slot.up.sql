CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS agent_task_graph_node_slot
    ON agent_task_queue (graph_run_id, work_native_id)
    WHERE graph_run_id IS NOT NULL
      AND (status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
           OR execution_uncertain);
