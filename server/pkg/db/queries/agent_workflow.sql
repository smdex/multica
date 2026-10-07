-- name: CreateAgentWorkflowRequest :one
INSERT INTO agent_workflow_request (
    id, workspace_id, requester_id, runtime_id, chat_session_id, task_id,
    run_id, turn_id, kind, status, request_hash, request, expires_at,
    native_source_id
)
VALUES (
    @id, @workspace_id, @requester_id, @runtime_id, sqlc.narg(chat_session_id),
    sqlc.narg(task_id), sqlc.narg(run_id), sqlc.narg(turn_id), @kind, 'pending',
    @request_hash, @request::jsonb, @expires_at, sqlc.narg(native_source_id)
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetAgentWorkflowRequest :one
SELECT * FROM agent_workflow_request
WHERE id = $1;

-- name: GetAgentWorkflowRequestForRequester :one
SELECT * FROM agent_workflow_request
WHERE id = $1 AND workspace_id = $2 AND requester_id = $3;

-- name: GetAgentWorkflowRequestForRuntime :one
SELECT * FROM agent_workflow_request
WHERE id = $1 AND runtime_id = $2;

-- name: LockAgentWorkflowRequestForRuntime :one
SELECT * FROM agent_workflow_request
WHERE id = $1 AND runtime_id = $2
FOR UPDATE;

-- name: GetActiveNativeImportWorkflowRequest :one
SELECT * FROM agent_workflow_request
WHERE workspace_id = $1
  AND requester_id = $2
  AND runtime_id = $3
  AND kind = 'native_session_import'
  AND native_source_id = $4
  AND status IN ('pending', 'running', 'unknown')
ORDER BY created_at ASC
LIMIT 1;

-- name: ListRecentNativeSessionListRequests :many
SELECT * FROM agent_workflow_request
WHERE workspace_id = $1
  AND requester_id = $2
  AND runtime_id = $3
  AND kind = 'native_session_list'
  AND status = 'completed'
  AND result IS NOT NULL
  AND created_at >= now() - interval '15 minutes'
ORDER BY created_at DESC
LIMIT @max_requests::int;

-- name: PurgeExpiredNativeSessionListResults :exec
-- Keep the idempotency record but drop private handles and the list payload
-- once its public session references have expired.
UPDATE agent_workflow_request
SET result = NULL, updated_at = now()
WHERE kind = 'native_session_list'
  AND result IS NOT NULL
  AND created_at < now() - interval '15 minutes';

-- name: ExpirePendingAgentWorkflowRequestsForRuntime :many
UPDATE agent_workflow_request
SET status = 'failed',
    error = '{"code":"expired","message":"workflow request expired before dispatch"}'::jsonb,
    updated_at = now()
WHERE runtime_id = $1
  AND status = 'pending'
  AND expires_at <= now()
RETURNING *;

-- name: ListPendingAgentWorkflowRequests :many
-- Candidate discovery is not admission. The handler locks the parent lifecycle
-- rows, checks the current invocation policy, then locks and claims each row.
SELECT * FROM agent_workflow_request
WHERE runtime_id = $1 AND status = 'pending'
ORDER BY created_at, id
LIMIT @max_commands::int;

-- name: DispatchAgentWorkflowRequest :one
UPDATE agent_workflow_request
SET status = 'running', dispatched_at = now(), updated_at = now()
WHERE id = $1 AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: LockWorkflowMember :one
SELECT * FROM member
WHERE workspace_id = $1 AND user_id = $2
FOR SHARE;

-- name: CompleteAgentWorkflowRequest :one
UPDATE agent_workflow_request
SET status = @status,
    result = sqlc.narg(result)::jsonb,
    error = sqlc.narg(error)::jsonb,
    updated_at = now()
WHERE id = @id
  AND runtime_id = @runtime_id
  AND status IN ('pending', 'running', 'unknown')
RETURNING *;

-- name: ExpireAgentWorkflowRequest :one
UPDATE agent_workflow_request
SET status = CASE WHEN dispatched_at IS NULL THEN 'failed' ELSE 'unknown' END,
    error = CASE
        WHEN dispatched_at IS NULL THEN '{"code":"expired","message":"workflow request expired before dispatch"}'::jsonb
        ELSE '{"code":"expired","message":"workflow request delivery is unknown"}'::jsonb
    END,
    updated_at = now()
WHERE id = $1
  AND status IN ('pending', 'running')
  AND expires_at <= now()
RETURNING *;

-- name: CreateTaskInteraction :one
INSERT INTO task_interaction (
    id, workspace_id, runtime_id, chat_session_id, task_id, run_id, turn_id,
    kind, status, request, expires_at
)
VALUES (
    @id, @workspace_id, @runtime_id, @chat_session_id, @task_id, @run_id,
    @turn_id, @kind, 'pending', @request::jsonb, @expires_at
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetTaskInteractionInChatSession :one
SELECT * FROM task_interaction
WHERE id = $1 AND workspace_id = $2 AND chat_session_id = $3;

-- name: LockTaskInteractionInChatSession :one
SELECT * FROM task_interaction
WHERE id = $1 AND workspace_id = $2 AND chat_session_id = $3
FOR UPDATE;

-- name: ListUnsettledTaskInteractions :many
SELECT * FROM task_interaction
WHERE workspace_id = $1
  AND chat_session_id = $2
  AND status IN ('pending', 'resolving', 'unknown')
ORDER BY created_at ASC;

-- name: GetPendingTaskInteractionForRun :one
SELECT EXISTS (
    SELECT 1 FROM task_interaction
    WHERE task_id = $1
      AND run_id = $2
      AND status IN ('pending', 'resolving', 'unknown')
) AS pending;

-- name: StartTaskInteractionResolution :one
UPDATE task_interaction
SET status = 'resolving',
    version = version + 1,
    response_request_id = @response_request_id,
    response = @response::jsonb,
    updated_at = now()
WHERE id = @id
  AND status = 'pending'
  AND expires_at > now()
RETURNING *;

-- name: ResolveTaskInteraction :one
UPDATE task_interaction
SET status = 'resolved', version = version + 1, updated_at = now()
WHERE id = @id
  AND status IN ('resolving', 'unknown', 'cancelled', 'expired')
  AND response_request_id = @response_request_id
RETURNING *;

-- name: MarkTaskInteractionUnknown :one
UPDATE task_interaction
SET status = 'unknown', version = version + 1, updated_at = now()
WHERE id = @id
  AND status = 'resolving'
  AND response_request_id = @response_request_id
RETURNING *;

-- name: RestoreTaskInteractionPending :one
UPDATE task_interaction
SET status = 'pending',
    version = version + 1,
    response_request_id = NULL,
    response = NULL,
    updated_at = now()
WHERE id = @id
  AND status IN ('resolving', 'unknown')
  AND response_request_id = @response_request_id
  AND run_id = @run_id
  AND expires_at > now()
RETURNING *;

-- name: CancelTaskInteractionsForRun :many
UPDATE task_interaction
SET status = 'cancelled', version = version + 1, updated_at = now()
WHERE task_id = $1
  AND run_id = $2
  AND status IN ('pending', 'resolving', 'unknown')
RETURNING *;

-- name: ListChatWorkflowTaskIDs :many
SELECT id FROM agent_task_queue
WHERE agent_task_queue.chat_session_id = $1
  AND (EXISTS (SELECT 1 FROM task_interaction i WHERE i.task_id = agent_task_queue.id AND i.status IN ('pending', 'resolving', 'unknown'))
       OR EXISTS (SELECT 1 FROM agent_workflow_request r WHERE r.task_id = agent_task_queue.id AND r.status IN ('pending', 'running')))
ORDER BY id;

-- name: ListRuntimeWorkflowTaskIDs :many
SELECT id FROM agent_task_queue
WHERE agent_task_queue.runtime_id = $1
  AND (EXISTS (SELECT 1 FROM task_interaction i WHERE i.task_id = agent_task_queue.id AND i.status IN ('pending', 'resolving', 'unknown'))
       OR EXISTS (SELECT 1 FROM agent_workflow_request r WHERE r.task_id = agent_task_queue.id AND r.status IN ('pending', 'running')))
ORDER BY id;

-- name: RetireStaleTaskInteractions :exec
-- Caller holds the task row. Keep the response identity for a dispatched late
-- acknowledgement; a retired prompt can never return to pending.
UPDATE task_interaction i
SET status = CASE WHEN i.expires_at <= now() THEN 'expired' ELSE 'cancelled' END,
    version = version + 1, updated_at = now()
WHERE i.task_id = $1 AND i.status IN ('pending', 'resolving', 'unknown')
  AND (i.expires_at <= now() OR NOT EXISTS (
      SELECT 1 FROM agent_task_queue t
      WHERE t.id = i.task_id AND t.status = 'running'
        AND t.runtime_id = i.runtime_id AND t.chat_session_id = i.chat_session_id
        AND t.active_run_id = i.run_id
        AND t.control_state ->> 'run_id' = i.run_id::text
        AND t.control_state @> '{"active":true}'::jsonb
        AND t.control_state ->> 'turn_id' = i.turn_id
  ));

-- name: RetireStaleTaskWorkflowRequests :exec
-- Dispatched commands are never redelivered, even after run retirement. Their
-- result stays reconcilable via authenticated reports carrying the same ID.
UPDATE agent_workflow_request r
SET status = CASE WHEN r.dispatched_at IS NULL THEN 'failed' ELSE 'unknown' END,
    error = '{"code":"stale_turn","message":"the task input is no longer active"}'::jsonb,
    updated_at = now()
WHERE r.task_id = $1 AND r.status IN ('pending', 'running')
  AND (r.expires_at <= now() OR NOT EXISTS (
      SELECT 1 FROM agent_task_queue t
      WHERE t.id = r.task_id AND t.status = 'running'
        AND t.runtime_id = r.runtime_id AND t.chat_session_id = r.chat_session_id
        AND t.active_run_id = r.run_id
        AND t.control_state ->> 'run_id' = r.run_id::text
        AND t.control_state @> '{"active":true}'::jsonb
        AND t.control_state ->> 'turn_id' = r.turn_id
  ) OR (r.kind = 'interaction_response' AND NOT EXISTS (
      SELECT 1 FROM task_interaction i
      WHERE i.response_request_id = r.id
        AND i.status IN ('resolving', 'unknown') AND i.expires_at > now()
  )));

-- name: CancelTaskInteractionResponse :exec
UPDATE task_interaction
SET status = CASE WHEN expires_at <= now() THEN 'expired' ELSE 'cancelled' END,
    version = version + 1, updated_at = now()
WHERE response_request_id = $1 AND status IN ('resolving', 'unknown');

-- name: DeleteTaskInteractionsByChatSession :exec
DELETE FROM task_interaction WHERE chat_session_id = $1;

-- name: DeleteAgentWorkflowRequestsByChatSession :exec
DELETE FROM agent_workflow_request WHERE chat_session_id = $1;

-- name: DeleteWorkflowRequestsBySystemRuntimeAgents :exec
DELETE FROM agent_workflow_request
WHERE agent_workflow_request.chat_session_id IN (
    SELECT cs.id FROM chat_session cs JOIN agent a ON a.id = cs.agent_id
    WHERE a.runtime_id = $1 AND a.kind = 'system'
) OR (kind = 'native_session_import' AND request ->> 'agent_id' IN (
    SELECT id::text FROM agent WHERE runtime_id = $1 AND kind = 'system'
));

-- name: LockWorkflowRuntime :one
-- Teardown takes runtime before agent/task/session locks. Acquire it before
-- those parents too, fencing ownership/status changes and native publication.
SELECT * FROM agent_runtime WHERE id = $1 FOR SHARE;

-- name: ListWorkflowAgentsForRuntime :many
SELECT * FROM agent
WHERE runtime_id = $1 AND workspace_id = $2 AND archived_at IS NULL;

-- name: RetireUndeliverableTaskInteractionResponses :exec
UPDATE task_interaction i
SET status = CASE WHEN i.expires_at <= now() THEN 'expired' ELSE 'cancelled' END,
    version = version + 1, updated_at = now()
WHERE i.task_id = $1 AND i.status IN ('resolving', 'unknown')
  AND EXISTS (SELECT 1 FROM agent_workflow_request r
              WHERE r.id = i.response_request_id AND r.status = 'failed' AND r.dispatched_at IS NULL);

-- name: DeleteWorkflowRequestsBySystemAgent :exec
-- A system chat delete also removes import reservations whose future session
-- does not exist yet. Native sources and owned artifacts are never touched.
DELETE FROM agent_workflow_request r
WHERE r.kind = 'native_session_import' AND r.request ->> 'agent_id' = @agent_id::text
  AND EXISTS (SELECT 1 FROM agent a WHERE a.id = @agent_id::uuid AND a.kind = 'system' AND a.system_key LIKE 'agent_builder:%');

-- name: CompleteNativeImportWorkflowRequest :one
UPDATE agent_workflow_request
SET status = 'completed', chat_session_id = @chat_session_id,
    result = @result::jsonb, error = NULL, updated_at = now()
WHERE id = @id AND runtime_id = @runtime_id
  AND kind = 'native_session_import' AND status IN ('running', 'unknown')
RETURNING *;
