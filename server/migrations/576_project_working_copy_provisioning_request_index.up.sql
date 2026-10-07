CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_project_working_copy_provisioning_request
    ON project_working_copy (provisioning_request_id);
