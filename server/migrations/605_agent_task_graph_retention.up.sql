CREATE INDEX CONCURRENTLY IF NOT EXISTS agent_task_graph_retention
    ON agent_task_queue (graph_run_id)
    INCLUDE (work_source_id, agent_id, runtime_id)
    WHERE graph_run_id IS NOT NULL;
