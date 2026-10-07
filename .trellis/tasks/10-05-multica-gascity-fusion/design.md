# Native Multica swarm features: implementation design and bounded task plan

Date: October 7, 2026. Status: **design only, implementation and acceptance not performed**.

## 0. Decision and delivery boundaries

Extend Multica's existing `TaskService`, PostgreSQL queue, daemon lifecycle and shared UI. **Workspace = City, Project = Rig. There is no Organization layer.** GasCity supplies useful dependency, addressed-mail, handoff and recovery ideas, not the execution daemon or a competing coordinator. Multica is the intended execution owner from the first slice.

Keep PG Issues and their APIs unchanged. Beads work can stand alone or link to Issues. Default: Beads owns its work fields, PG owns collaboration Issues, and Multica PG owns native executions. Optional same-entity bidirectional synchronization is a later explicitly governed mode, not a mandatory migration. jj is the preferred repository execution mode. The first slice uses a no-repository controlled worker and makes no jj claim.

This plan applies ponytail full: reuse queue attempts, claim generations, workflow control requests, task tokens, transcript rows, Query hooks and shared views. Add no generic backend interface, second scheduler, organization model, formula marketplace, message broker, or separate session service. New file names below are concrete implementation targets, not files created by this planning session. Sixteen bounded tasks include the first slice, consolidation, jj and the previously requested GitLab roadmap.

Authoritative baseline: resolved waggle `p6sUTcif`, then read `/home/smaximov/.jcode/scratch/fusion-corrected-native-baseline-20261007.md`, SHA-256 `f5577bf161ce5222a6cdf09e8358c12c0826b9c3c3ce4c25949df739341cda41`. That token resolves a path reference, so this hash identifies the actual baseline bytes. October 6 reports remain historical evidence only where consistent with this baseline. No old report was changed.

## 1. Supported features and user flows

| Feature | MVP, first complete slice | Next scope, explicitly retained |
|---|---|---|
| Workspace work explorer | Open `/{workspaceSlug}/work`. See existing PG Issues, workspace-scoped Beads work and one project's Beads work together. Filter by project, source, kind, status and linked/unlinked. Display source identity and execution ownership. Open the existing Issue page for an Issue, source-aware detail for a bead. | A second project/source, stable federation paging, partial-outage and count accuracy before claiming consolidation complete, T12. |
| Project work | Existing project detail links to the work explorer with its project filter. An unassigned workspace source remains visible through the workspace filter, not silently assigned to a project. | Multiple source bindings per project are supported by identity, not assumed one store per repository. No cross-workspace federation. |
| Linked and standalone Beads work | Read and edit supported source-owned fields. Link or unlink an existing Issue without copying it. Create a source-owned task once source create attribution/readback passes T01. Standalone work creates no synthetic Issue. Show pending/ambiguous/failed source writes. | Same-entity two-way Issue sync, T14. Explicit conflict resolution, not last-write-wins. |
| Graph and run detail | Preview a bounded, acyclic dependency snapshot, select agents and capacity, then Start. A/B can run in parallel; C waits for success and named handoffs. View node readiness reasons, attempts, source revision, mail and sessions. | Editable reusable workflow definitions and dynamic graph expansion are not MVP. Source dependency edits affect the next run. |
| Mailbox | Address a run node, see sender attempt, recipient, thread/reply, delivery and processed acknowledgment separately. Offline recipient delivery survives restart. Human view is an operator view, not an agent impersonation API. | Cross-run addressing only if a concrete workflow needs it. No generic topic bus. |
| Handoffs | Publish typed, immutable output slots with source-attempt provenance. C sees exactly which A/B outputs were selected at admission and explicitly acknowledges consuming them. | Additional types can be added with versioned validators, not arbitrary executable payloads. |
| Run sessions | Open run/node/attempt Sessions tab. Read output captured before browser attach, watch live, reconnect and replay retained output. Acquire control, send text to the actual active process/turn. Browser close neither starts nor kills anything. | Optional tmux/herdr attach to that same process after Web acceptance, T16. Full VT screen emulation is not required for semantic text input. |
| Operator actions | Start, Hold, Resume, Cancel, Retry with server-confirmed outcomes. Hold stops new admission, not an OS process pause. Cancel remains “stopping” until actual stop or an explicit unknown-state result. Retry creates a new queue attempt after the previous process is known stopped. | Fine-grained node skip/force-success is excluded because it would bypass dependency evidence. |
| Repository execution | Explicit `none` for first controlled slice. Existing Git flows preserved and labeled where used. | Preferred jj lifecycle implemented by Multica's actual execenv, T13. No silent fallback from a selected jj mode to Git. |
| GitLab | Existing MR/CI behavior preserved. No new provider access in first slice. | Bidirectional Issue title/description/open-close-reopen, labels policy, assignee mapping disclosure, conflict/outage/confidentiality handling, T15. Comments inbound/display plus supported outbound-create first. Comment edit/delete and group-label administration later. |
| Platforms | Web-first acceptance, shared core/views wired to both Web and desktop. Existing mobile Issue behavior remains compatible. | No new mobile swarm screens are claimed or silently added to scope. A later mobile UI task needs its own requirements, AGENTS/spec read and independent checks. |

UX defaults: one main Start action on preview, visible readiness/ownership errors beside the action, no redundant help prose. Use existing Button/Dialog/AlertDialog contracts, semantic tokens, keyboard focus, labeled filters and scrollable transcript. Show unknown input delivery as unknown, not a Retry button that could write twice. Cancel/retry dialogs explain effects on live workers and external side effects. All new shared copy follows the existing English/Chinese conventions and translations.

## 2. Existing implementation seams and decisive findings

Evidence is a selective static read of the dirty working tree at HEAD `333166516f91ad404114c194ad5ced87eb8a4b2d`, not a clean-build claim. Paths and symbols are better anchors than line numbers as implementation progresses.

| ID | Existing seam read | Consequence for the design |
|---|---|---|
| E1 | `server/internal/service/task.go`: `EnqueueTaskForIssueWithHandoff`, `ClaimTask`, `ClaimTaskForRuntime`, `ClaimTasksForRuntimes`, `FinalizeTaskClaim`, `StartTaskForClaimWithRun`, `CompleteTaskWithTransition`, `MaybeRetryFailedTask`, `HandleFailedTasks`; `server/pkg/db/queries/agent.sql`: `CreateAgentTask`, `CreateRetryTask`, `ClaimAgentTask`, `LockAgentTaskStartClaim` | Queue rows already represent attempts. `dispatched_at` plus runtime is the existing claim generation. Reuse both. Issue/chat/null-shaped serialization must explicitly recognize graph-backed tasks; merely inserting all-null Issue/chat tasks would hit quick-create behavior. |
| E2 | `server/migrations/564_agent_workflow_state.up.sql`, `server/pkg/db/queries/agent_workflow.sql`, `server/internal/service/task_workflow.go` | Existing `active_run_id`, control state, durable `agent_workflow_request`, unknown delivery, task interactions and lifecycle retirement already exist. Do not propose another generic command or session database for Web input. |
| E3 | `server/internal/daemon/workflow.go`: `validateWorkflowTaskPolicy`, `registerWorkflowRun`, `workflowRun`, `steer`; `server/pkg/agent/agent.go:Session`; `server/pkg/agent/interactions.go` | `Session.Steer` is a same-live-session callback. Registration currently requires chat mode and that mode requires a chat session. Extend the same machinery to graph tasks without inventing chats or launching an input-helper agent. `InputDelivery` explicitly treats uncertain provider writes as terminal for that command ID. |
| E4 | `server/internal/handler/agent_workflow.go`: `InitiateChatSteer`, `lockWorkflowParents`, request dispatch/result handling; `server/cmd/server/router.go` | Existing interactive APIs are chat-scoped. New task-scoped entry points must share their checks and request storage while preserving old chat endpoints. Existing runtime invocation and membership rules must not be weakened. |
| E5 | `server/internal/daemon/daemon.go` around 9586: flush clears `batch` before reporting and logs failure; `server/pkg/db/queries/task_message.sql`: plain batch INSERT; `server/internal/handler/daemon.go:ReportTaskMessages` | Producer delivery can lose a batch on report failure. SQL comments explicitly identify missing retry/uniqueness. A UI dedupe cannot fix this. Add durable spool plus server idempotency before claiming retained replay. Not reproduced here. |
| E6 | Same message handler around 5180; user catch-up `ListTaskMessagesByUser`; `packages/core/chat/queries.ts`; `packages/core/realtime/use-realtime-sync.ts` | Live broadcast currently only enables Issue/chat-backed tasks. Standalone Beads execution needs graph-based workspace resolution, scoped fanout and catch-up, not an assumption that existing streaming covers it. |
| E7 | `server/internal/handler/task_lifecycle.go:RecoverOrphanedTasks`, `PinTaskSession`; `server/cmd/server/runtime_sweeper.go`; `server/internal/service/task.go` retry/fail/rerun insertion sites | Startup recovery and automatic/manual retry can create attempts outside a new Start handler. All must respect graph hold/cancel/readiness and active-attempt fencing. Pin/progress/completion/output also need incarnation checks for graph tasks. |
| E8 | `server/migrations/108_task_token.up.sql`, `server/internal/middleware/auth.go`, `workspace.go` | Existing task tokens bind authenticated task/agent/workspace identity. Extend graph checks from this identity. Never accept `from`, workspace or attempt headers supplied by agent content as authority. Historical migration FKs are not permission to add new FKs. |
| E9 | `packages/core/api/schema.ts`, `schemas.ts` task-message/control schemas, `ws-client.ts`, `types/events.ts` | HTTP uses `parseWithFallback`; WS transport currently parses/casts an envelope. New payloads require actual runtime validation before cache writes. Unknown enum values must fail safe for controls, not crash older clients. |
| E10 | `apps/web/app/[workspaceSlug]/(dashboard)/projects/[id]/page.tsx`, desktop `routes.tsx`, shared transcript components | Reuse shared views and navigation adapters. Workspace routes, not global organization pages. Add desktop destinations when adding shared Web pages. |
| E11 | `server/pkg/db/queries/project_resource.sql`, `daemon/execenv/local_worktree.go`, `execenv/git.go`, `repocache/cache.go`, `daemon/gc.go` | Project resources already carry execution context. Beads source binding is not a replacement project/repository ID. jj must preserve existing provenance, dirty-work protection, resume and cleanup semantics, not replace one command string. |
| E12 | `server/internal/integrations/vcs/gitlab.go`, `server/internal/handler/vcs_webhook.go` | Current GitLab paths normalize MR/pipeline events. Extend them without claiming existing issue sync. Deployment-specific issue create/dedupe and confidentiality behavior remains unqualified. |

Read AGENTS.md, `.trellis/workflow.md`, core/views/web/desktop spec indexes, core state/type specs and cross-layer/reuse guides. The layer indexes and sampled core specs are mostly templates, so AGENTS.md and current code are the operative rules. No backend-specific spec index was found in the enumerated spec tree. Read UI Button/Dialog contracts and naming/i18n conventions for proposed UI paths. Planning remains in this scratch artifact because repository writes and task scaffolding are explicitly prohibited.

## 3. Minimal ownership and persistent records

### 3.1 IDs and tenancy

Existing workspace, project, Issue, agent, runtime and task UUIDs remain unchanged. Use `dbid.NewV7` for new application IDs where the repository does. A bead's identity is `(workspace_id, source_id, native_id)`, never a bare bead ID or mutable URL. Display identifiers may be shortened but API keys/cursors contain full scope. Project is nullable for workspace work and must belong to the same workspace when present. No invented per-project membership system.

Avoid the existing overloaded “run” terminology: API `graph_run_id` identifies the dependency run; `task_id` identifies a queue attempt; existing `active_run_id` is the process/foreground execution **incarnation** and is called `incarnation_id` in new public session payloads. Existing `Task.WorkflowRunID`/workflow control `run_id` continue to mean that incarnation, not the graph run. Do not repurpose them.

### 3.2 Record changes

These are the minimum proposed durable additions for the selected features. Implementers should reuse an equivalent current record discovered during T01 rather than introduce a duplicate.

| Record | Owner and minimum changes | Why it must persist |
|---|---|---|
| Existing `issue`, comments, project/resources | Existing PG services remain owners. No source flag that turns an Issue into a read-only projection. | Compatibility and independent human collaboration. |
| **New `work_source`** | PG owns UUID, workspace UUID, nullable project UUID, display name, configured daemon UUID/opaque approved source handle, mode `observe` or `native`, enabled flag, configuration revision and last health/error. Beads owns task fields/dependencies. No secret or arbitrary executable/path in browser config. | Stable scope, permission-safe source routing and explicit execution ownership. Multiple bindings can map different project contexts, but the same physical source must not be independently registered as two execution owners. |
| **New `issue_bead_link`** | PG owns link UUID plus `(workspace_id, issue_id, source_id, native_id)`, creator and time. Many-to-many links allowed. Validate both ends belong to workspace. | A relationship, not a duplicated Issue. Unlink has no delete or status side effect. |
| **New `work_source_command`** | PG owns command UUID, workspace/source IDs, actor, operation, target native ID, expected revision, canonical request hash/body, state, source receipt/revision, error, timestamps. Limited to selected Beads writes/status repair, not a generic backend bus. | PG and Dolt cannot commit atomically. Stable write attribution, lost-reply recovery and visible ambiguity are required. |
| **New `workflow_run`** | PG/TaskService owns UUID, workspace, nullable project, source/root identity, immutable graph JSON/version/source revision, node-state JSON, capacity, status, revision, start-request UUID/hash, creator and timestamps. | Durable dependency/join state and operator state. Freeze graph; no separate workflow-definition table for MVP. |
| Existing `agent_task_queue` | Add nullable `graph_run_id`, `graph_node_key`, `work_source_id` and `work_native_id`, with source/native identity validated against the frozen node. Reuse task UUID, attempt/max_attempts, parent/retry lineage, runtime, status, claim generation, `active_run_id`, session/workdir and control fields. Add `execution_uncertain BOOLEAN NOT NULL DEFAULT false` for graph process-liveness reservation. Add nullable controller user UUID, lease UUID and lease deadline if not already equivalent. | One attempt owner, one queue. Unique active slots per graph/node and per source/native work item are enforced in SQL, including across overlapping runs. Existing `attempt` is per node lineage, not graph-wide. |
| **New `agent_mail`** | PG owns UUID, workspace, graph_run_id, sender task/incarnation, recipient node key, thread UUID, optional reply-to UUID, send-request UUID/hash, bounded content, persisted/expired state and delivery/processed receipt fields including receiving task/incarnation and timestamps. One recipient per message. | Transcript and comments have no durable addressed-processing semantics. One row suffices because only one live recipient attempt is allowed per node. Archived delivery history may be a bounded JSON receipt array when reassignment occurs. |
| **New `task_handoff`** | PG owns UUID, workspace, graph_run_id, producer task/incarnation, slot/type/schema version, payload/hash and timestamp. Immutable per producer-attempt/slot. | Typed output evidence must not disappear with logs or be overwritten by a retry. |
| Handoff consumption | In the already locked `workflow_run.node_state`, record selected input handoff IDs/hashes and a consumer task/incarnation receipt per slot. No separate table for a bounded MVP graph. | Admission pins inputs atomically; explicit consumed ack is separate. Consumer retry gets a new receipt even if payload is unchanged. |
| Existing `task_message` | Add nullable incarnation ID and immutable producer sequence/digest as needed. Reuse existing `seq` as producer cursor if T01 confirms one incarnation per graph attempt. Idempotency key is `(task_id, incarnation_id, seq)`. Keep legacy rows/endpoints compatible. | Durable server replay and retry-safe batch ingest. Do not indiscriminately dedupe historical rows by seq where identity is ambiguous. |
| Daemon transcript spool | Actual daemon owns an append-only file per task/incarnation under its managed data root, persisted sequence and server high-water acknowledgment. Not a new service/table. | Output survives request failure and daemon restart before server commit acknowledgment. |
| Existing `agent_workflow_request` | Reuse task/runtime/run/turn IDs, request hash, status/result/unknown handling for text input. Permit task-scoped steering without chat session. Add controller lease ID to validated request body. | Existing durable control identity already covers the required command lifecycle. |

Bounded graph default: at most 128 nodes, 512 dependency edges and 2 concurrent workers, with run capacity never overriding current runtime/agent capacity. Lock one `workflow_run` row for readiness, current node attempts, chosen handoffs and state transition. This avoids node/edge/consumption tables and a distributed scheduler. Add an English `ponytail:` comment naming the O(nodes+edges) scan and per-run lock ceiling; normalize only after measured contention or a requirement beyond the bound. Invalid oversized/cyclic graphs are rejected, not truncated.

All new relationship validation and dependent cleanup is application code in transactions. **No new foreign keys, cascades or implicit migration-created indexes.** Create each primary/unique lookup index with its own single-statement `CREATE [UNIQUE] INDEX CONCURRENTLY` migration, then attach constraints separately if needed. Include partial unique indexes for active graph-node attempts and source command/send request dedupe. Use idempotent DDL where touching conditionally created objects. Run `make sqlc` after SQL changes during implementation.

### 3.3 Source integration default and qualification

Use one concrete Beads/Dolt mode through the existing trusted daemon command transport and configured source directory, not browser-to-Dolt access or a hypothetical `WorkStore` interface. The first candidate is a **pinned `bd` JSON command mode against one dedicated Dolt-backed source**, with executable/path fixed by operator configuration and argv constructed without a shell. Default tests use a test-created executable, not installed tools. Do not import private Beads internals or issue raw writes against an unverified schema.

T01 must qualify its snapshot revision, stable list cursor, update precondition, create attribution/readback, error and ownership behavior with a disposable source later. If the selected public mode cannot atomically enforce edits against a revision while external editors operate, expose read-only mode until a supported conditional write path is implemented. A connector mutex alone is not a source CAS. This is a bounded implementation prerequisite, not permission to quietly weaken conflict handling. Source read-only exploration and native run execution can progress independently; the complete edit/create feature is not accepted until the gate passes.

Default ownership is a dedicated native-managed source, externally scheduled roots are observe-only. Before enabling native dispatch for an existing externally controlled source/root, stop its external scheduler, verify no live leases/processes, and record the source identity and quiescence evidence. If external ownership cannot be verified, refuse Start. A PG label cannot fence an unrelated GasCity process. No automatic owner-transfer product is built in MVP.

## 4. State machines, transaction boundaries and recovery

### 4.1 Graph and attempts

Run states: `draft -> running -> succeeded|failed`; `running -> held -> running`; any nonterminal active run may enter `cancelling -> cancelled`. Terminal failure/cancel never silently resumes. Explicit `failed -> running` retry is allowed only for a retryable failed node with all previous processes known stopped, valid prerequisites and remaining attempt budget. It creates a new attempt within the same frozen graph and increments run revision. Retry on a held run keeps it held and queues no process until Resume. Successful nodes are not rerun by Retry in MVP; re-executing a successful graph creates a new run. Cancelled runs are cloned to a new run, not reopened with old inbox/session identities.

Node state is `blocked`, `ready`, `queued`, `dispatched`, `running`, `succeeded`, `failed`, `cancelled` or `unknown`. Ready means all required predecessor attempts succeeded and their required typed handoffs exist, are valid and selected. Unreachable successors remain blocked with a concrete reason; a run with exhausted failure settles failed, not indefinitely running. A node claiming success without its required outputs settles as a failed/incomplete-output attempt, not successful readiness evidence.

One TaskService operation, implemented next to current task lifecycle code, locks the run, checks workspace/agent invocation, ownership, hold/cancel, graph revision, readiness and capacity, inserts a queue attempt and records the current node attempt together. Reuse SQL queue claims and daemon execution. Claims reserve capacity for dispatched/preparing/running workers and unresolved possibly-live executions, not only processes already marked running. Recheck admission under the same run-before-task lock order at actual start so Hold/Cancel wins against already queued work. Waiting-local-directory occupies a reservation until released, and must not be mistaken for a started process.

Attempts reuse existing queue status and retry lineage. Every graph attempt requires `(task_id, runtime_id, dispatched_at claim generation, incarnation_id)` on start, result, control, pin and progress mutations, as appropriate. Agent calls additionally use the task token. Stale results return conflict and cannot complete a newer attempt. An old transcript batch may be accepted into its **own historical incarnation** under its original authenticated runtime fence and closed high-water rules, but never change current state or append into a successor. Missing new fence fields are not accepted for graph tasks, while legacy installed-daemon semantics remain supported for legacy tasks. Capability negotiation prevents an old daemon from claiming a graph task it cannot safely execute.

Actual process launch is not atomic with PG Start. Daemon writes its task/incarnation launch reservation locally, registers the incarnation with fenced Start, launches at most once for that reservation, and records its supervised process identity. Duplicate dispatch on that live daemon reuses the reservation, not `Execute`. After a daemon crash, local reservation/PID evidence must distinguish “never launched” from “possibly alive.” Do not start a replacement until previous process tree termination or adoptability is established. When survival cannot be proven, show unknown and block automatic retry. Arbitrary external tool side effects are not exactly-once.

Hold prevents further claims/starts but lets active workers finish and report output/mail. Resume reevaluates readiness. Cancel first commits `cancelling`, revokes control and blocks new start/mail sends/handoffs, then signals existing daemon cancellation. In-flight completed records that committed before cancellation remain history, but cannot unlock successors after the cancel revision. `cancelled` requires stop acknowledgment or verified dead process, not a lost heartbeat. Unknown liveness stays visible and retry-blocking. Coordinator restart rehydrates run/queue state without clearing valid live leases. Daemon startup and runtime sweeper use the same graph-aware failure/retry path.

### 4.2 All admission and retry paths to fence

Implementation must enumerate every `CreateAgentTask`, `CreateRetryTask`, graph enqueue and claim caller. Initial static anchors include:

- Issue runs and `issue_trigger.go`, mentions, thread-parent, squad leader and `EnqueueTaskForIssueWithHandoff`/squad handoff.
- Quick-create/source-context retry, chat enqueue and prepared chat transactions.
- Autopilot/webhook/recovery task creation, delegated runs, manual rerun, fail-and-create-retry, `MaybeRetryFailedTask` and `HandleFailedTasks`.
- Single-agent/runtime/batch claims, claim delivery finalization and failed delivery requeue.
- Legacy unfenced `StartTask`/`StartTaskWithRun` alongside claim-qualified starts.
- Daemon recover-orphans and `runtime_sweeper.go`, cancellation bulk paths and session-pin/progress/completion writers.

For a graph-linked task, each path invokes the shared graph gate or rejects it with “use graph control.” An Issue merely linked to a bead can still have unrelated legacy runs. It must not implicitly re-enqueue that bead root through an Issue trigger. A partial unique active-run index on `(workspace_id, source_id, root_native_id)` blocks duplicate starts of the same root under different request IDs. A second queue index reserves `(work_source_id, work_native_id)` across overlapping graph runs, so two roots cannot execute a shared bead concurrently. Busy work yields explicit blocked readiness, never a second attempt. Unknown-liveness attempts keep their slot reserved even if the legacy queue status has settled failed, using an additive `execution_uncertain` boolean in the active-slot predicate until verified stopped. A partial unique active graph-node index is the last defense, not a replacement for checking graph policy. The full caller inventory is T04 acceptance, not a claim that this planning read audited every caller.

### 4.3 Mail and handoffs

Mail send: authenticated live attempt + client request UUID -> one durable message row -> `pending_delivery`. Same sender/request/hash returns the same message. Same request UUID/different body is conflict. Sender is server-derived, recipient is a node in the same run/workspace. Replies must reference a message in that scoped thread and use its immutable thread ID. Content is bounded data, not permission to call tools, alter graph policy, approve a workflow or impersonate a human.

Agent mailbox pull delivers a message to the current authorized recipient attempt and returns a receipt token bound to message/task/incarnation. `delivered` means the recipient accepted the delivery, not processed it. `processed` requires an explicit acknowledgment with the receipt and optional result summary. Duplicate acks are idempotent. Lose the ack response and the same receipt can be retried. If the recipient dies before processed ack, keep the logical message, fence the old receipt and redeliver to the replacement recipient attempt. Already processed messages remain visible as processed and are not silently processed anew on retry. Applications requiring a rerun of an action send a new message. Delivery is at least once, logical receipt transitions are idempotent, arbitrary recipient effects are not exactly once.

Default recipient addresses are stable run node keys rather than transient PID or display names. No human sending as an agent. Operator notes, if later needed, use explicit human actor provenance. No execution instruction is automatically run merely because it arrived in mail.

Handoff shape v1: `{id, graph_run_id, producer_task_id, incarnation_id, slot, type:"result.v1", summary, outcome, artifacts:[{kind, reference, digest?}], validation, open_questions}`. Server validates type/size/artifact reference syntax and source scope. Never fetch arbitrary artifact URLs automatically or execute supplied paths. A handoff is immutable once accepted; same slot/body is idempotent, differing body is conflict. Publish before task success settlement, or bundle publish+success in one PG transaction. Admission of C atomically stores A/B selected handoff IDs in its node state and queue context. C fetches those inputs before useful work and posts consumed acks. **Consumed ack cannot be a prerequisite for C's process start**, which would deadlock. It is required before C can report success. No required handoff is garbage-collected while its run/consumer receipts remain retained.

### 4.4 Source commands and cross-store ambiguity

Source command states: `pending -> applying -> applied|conflict|failed|unknown`. PG records intent and request hash before any source write. A selected source receipt/operation attribute proves apply where available. A timeout after possible source commit is unknown. Read back the operation identity and revision, then settle applied or conflict. Retry only on definite non-application or receiver-side dedupe. Never blindly replay creates or deletes. An unresolved create remains visible for operator repair rather than producing a duplicate bead.

A native task result commits its execution outcome and source-status command in one PG transaction. Source repair retries separately. UI may show “execution succeeded, source update pending/conflicted.” Beads being manually closed does not mark the PG attempt successful, and successful workflow execution does not silently close a linked human Issue. An outage after graph snapshot does not rewrite that snapshot. Default is to hold new admissions while source ownership/configuration is unavailable or changed, while capturing and settling already running attempts safely.

For later same-entity sync, last-synchronized base and per-side revisions determine one-sided edits. Shared-field edits on both sides create a visible conflict preserving both values. Conditional writes protect concurrent external edits. Echo suppression uses operation/version provenance, not timestamps. Missing list entries are not deletion tombstones. Assignee identities require explicit mapping. No unsupervised second sync engine for the same association.

### 4.5 Session capture, replay and input

Reuse the actual `agent.Session` from `runTask`/Execute and current workflow run registry. For graph tasks, registration is enabled without `chat_session_id`; retain current chat behavior. Do not use transcript import, replay or native-session resume to pretend that a new process is the current process. Same-session input exists only while the daemon holds that exact live Session callback and matching incarnation/turn. Unsupported adapters show observation-only mode. Supporting all installed providers is not an MVP acceptance claim.

Capture normalized text/tool/control-visible output from process start, before browser attach. Persist bounded producer records to a daemon spool before considering them durably captured, then report batches. Keep records until server acknowledges contiguous committed high-water. Replay after network/restart with `(task, incarnation, seq)` dedupe and payload-digest conflict rejection. Preserve current redaction and NUL sanitization. Local spool permissions are private and retention honors secret handling; server stores redacted output. Explicitly distinguish raw OS stdout/stderr from adapter-normalized transcript. MVP promises captured normalized output, not every byte of terminal screen state. Controlled worker acceptance also captures its error stream or clearly labeled diagnostic output.

Spool/full-disk failure must not silently discard acknowledged output. Fail closed for durable-capture capability, stop/hold the task as supported and surface a capture error. Do not claim host disk destruction survival. The guarantee is no loss of server-acknowledged records and replay of records fsynced to the available spool; bytes still in an OS pipe during a crash cannot be promised. Terminal session metadata includes final high-water so a closed run can still finish ingesting its own spool without reopening execution.

New replay cursor: `{task_id, incarnation_id, after_seq}`. Return ordered records, `next_seq`, retained minimum and final/available high-water. Default retained output is 7 days after terminal settlement, configurable by existing deployment config, with active/unacknowledged spool excluded from premature GC. Expired cursor returns explicit `cursor_expired` plus retained minimum, never an empty “complete” list. Subscribe then catch up through a captured high-water; merge idempotently by cursor and refetch gaps. WS is a notification/low-latency path, not the durable log.

Control is a single controller lease per active queue attempt: default 30 seconds, renewed at 10 seconds while controlling. Existing members with agent invocation permission can request it, not observers or task-token agents. Competing controller gets conflict and must wait for release/expiry in MVP, rather than inventing an unverified administrator takeover permission. Server time is authoritative. Each text input includes request UUID, expected incarnation/turn and lease ID. Check membership, invocation, run state and lease both at enqueue and daemon dispatch, not just when the page opened. Cancellation, incarnation change and membership revocation invalidate the lease.

Reuse `agent_workflow_request` pending/running/completed/failed/unknown and same request-hash rules. A provider-acknowledged input is delivered. A write followed by lost provider ack is unknown and **must not be replayed automatically**, even after control lease reacquisition. A new request ID is not a safe generic retry. Text input is not an instruction to restart the agent. Terminal attach later must use the same arbitration or remain observation-only.

## 5. Concrete API, event and agent contracts

### 5.1 Public HTTP additions

Paths below are proposed additions within the current `/api` workspace-selected router. Use existing authenticated workspace middleware and `X-Workspace-ID`, not a new tenancy system. Every object query filters workspace and verifies project/source membership. UUID-or-human-readable Issue URLs continue through existing loaders. New pure UUID request fields use `parseUUIDOrBadRequest`.

| Endpoint | Contract and result |
|---|---|
| `GET /api/work-sources` | Authorized source metadata/health only, never secret handles. Existing workspace admin policy governs source config mutation. |
| `GET /api/work?project_id=&source_id=&kind=&cursor=` | Discriminated Issue/bead rows with full scoped identity and source status. Composite scoped cursor includes query fingerprint and per-source progress. Counts carry complete/partial and stale/source error state, not misleading zero. |
| `GET /api/work-sources/{sourceId}/tasks/{nativeId}` | Source revision, fields, dependencies, links and source capabilities. Native ID encoded as opaque path segment, not interpolated shell text. |
| `POST /api/work-sources/{sourceId}/commands` | `{request_id, operation, native_id?, expected_revision?, fields}` -> 202 durable receipt. Allowlist create/update/status only after source qualification. Request ID/hash mismatch 409. |
| `GET /api/work-sources/{sourceId}/commands/{requestId}` | Persisted applied/conflict/failed/unknown result for recovery. |
| `POST /api/issues/{issueId}/bead-links`, `DELETE .../bead-links/{linkId}` | Validate both scoped records. Link/unlink only. Wait for server before closing UI. |
| `POST /api/workflow-runs` | `{request_id, source_id, root_native_id, expected_source_revision, project_id?, agent_assignments, capacity}` -> frozen draft snapshot + readiness/validation errors. Source snapshot is fetched by server/daemon, not trusted from browser. |
| `GET /api/workflow-runs/{graphRunId}` | Graph/version, node readiness, attempt IDs, source reconciliation state, permissions/capabilities and run revision. |
| `POST /api/workflow-runs/{graphRunId}/actions` | `{request_id, expected_revision, action, node_key?}`, with action one of start, hold, resume, cancel, retry. Persist request receipt in run command history, return same result on request replay. Bound accepted action count to 1024 per run and reject further new actions except cancellation through a reserved slot, rather than evict dedupe identities during run retention. CAS conflict 409. Retry never bypasses process-stop or dependency gates. |
| `GET /api/workflow-runs/{graphRunId}/mail`, `GET .../handoffs` | Operator-visible paged records and receipt states, under workspace access. Agent pull uses stricter recipient scope below. |
| `GET /api/tasks/{taskId}/session` | Current incarnation, liveness, normalized-output capabilities, retention cursor and controller state. No start/attach side effects. |
| `GET /api/tasks/{taskId}/messages?incarnation_id=&after_seq=` | Extend existing user replay endpoint with additive incarnation/cursor semantics. Legacy request/response remains supported. |
| `POST /api/tasks/{taskId}/control-lease` | Acquire/renew/release with request UUID and expected incarnation, server-generated lease token. |
| `POST /api/tasks/{taskId}/steer` | `{request_id, incarnation_id, turn_id, lease_id, content}` -> existing workflow request receipt. Generalize current chat steering internals, do not copy them. |

Invalid input 400, unauthorized 401, forbidden/non-disclosing not-found according to existing policy, stale revision/lease/incarnation or ownership conflict 409, unsupported operation 422, source outage 503 where no durable command was accepted. Accepted asynchronous commands return 202 and a pollable receipt even if upstream delivery is pending. Control buttons do not infer success from a generic parse fallback: malformed result disables control and triggers refetch/error state.

### 5.2 Daemon and agent additions

Extend current daemon heartbeat/pending-command and result routes only for the concrete source operations and session controls needed. `server/pkg/protocol/` and daemon client must carry graph/task/source IDs and capability versions. No arbitrary remotely supplied command strings. Source inspection executes under an approved directory handle and fixed executable. Daemon workspace authorization precedes path resolution.

Agent endpoints under the existing task-token-authenticated API:

- `POST /api/workflow-runs/{id}/mail`: `{request_id, recipient_node_key, thread_id?, reply_to_id?, content}`. Derive sender, graph and incarnation from authenticated current attempt, reject impersonating fields.
- `GET /api/workflow-runs/{id}/mail/inbox?after=`: only messages addressed to this attempt's node, plus durable receipt state. Pagination is mailbox scope, not global workspace scope.
- `POST /api/workflow-runs/{id}/mail/{messageId}/acks`: `{receipt_token, state:delivered|processed, result?}`. Current recipient task/incarnation fence, idempotent ack.
- `POST /api/tasks/{taskId}/handoffs`: typed v1 payload excluding server-authoritative producer/workspace fields. Only this live authenticated task may publish.
- `GET /api/tasks/{taskId}/inputs`: pinned predecessor handoff references and scoped payloads for this attempt.
- `POST /api/tasks/{taskId}/inputs/{handoffId}/consumed`: idempotent receipt tied to this task/incarnation. Require before successful completion.

Reuse injected task token credentials, not a daemon owner PAT. Bind graph credentials to the active incarnation or require a server-side current-incarnation check on every request. Revoke/fence old task tokens after cancellation or retry. Avoid putting mail content into privileged prompt/instruction channels without explicit untrusted-data framing. Existing tools can call these HTTP endpoints through the current task API; no new agent framework is necessary.

### 5.3 Realtime and client state

New additive events: `work_source:changed`, `workflow_run:changed`, `agent_mail:changed`, `task_handoff:created`, `task_session:changed`. Each uses `{workspace_id, object_id, revision, graph_run_id?, task_id?, incarnation_id?}` and carries only scope-safe data needed for invalidation. Existing `task:message` gains optional incarnation and cursor fields for graph output, with the old shape preserved for legacy tasks. New graph messages must not reach old Issue transcript cache entries merely because a task ID is present.

Publish only after commit. A lost WS event is repaired by reconnect/query refetch and sequence catch-up. Reuse realtime Hub membership routing and test revocation on an already-open socket. Never put privileged source handles or unredacted agent content in workspace-wide generic events. Mail content should be fetched through its scoped endpoint rather than broadcast as a global payload.

Add Zod response and event schemas in `packages/core/api/schemas.ts`, use `parseWithFallback` in `client.ts`, validate new WS envelopes/payloads before dispatch/cache mutation, and preserve unknown-event tolerance for installed clients. Query keys begin with workspace ID and include graph/source/task/incarnation as applicable. TanStack Query owns all fetched work, graphs, mail, receipts and session history. Zustand may own filters/layout/drafts/controller UI intent, not server payloads or an authoritative lease. Clear/refresh on workspace switch. Shared views use `useNavigation`/`AppLink`, never framework routing imports.

## 6. Dependency-ordered implementation tasks

P0 is required for the first complete slice (T01-T11), not one commit. P1 is required next consolidation/jj scope (T12-T13). P2 retains the later selected sync/GitLab roadmap and optional terminal attach (T14-T16). Risk names describe implementation uncertainty, not authorization to skip checks. All checks below are **future acceptance requirements, not tests run**.

### T01. Qualify the selected source and live-session slice
- **Outcome:** a small written executable contract for one Beads/Dolt source mode and one same-process controllable worker, with no architecture reopening.
- **Dependencies:** none. **Priority/risk:** P0, high. **Qualification prerequisite:** explicit permission for future disposable source/process fixtures, never installed/account-backed agent discovery.
- **Targets:** existing `server/internal/daemon/workflow.go`, `server/pkg/agent/agent.go`, `interactions.go`, queue/message SQL; new bounded fixture tests beside daemon/service. No product source abstractions.
- **Change/substeps:** (a) inspect the pinned source's public JSON operations and implement a test-only contract fixture for revision/list/update/create recovery, (b) test-created worker emits PID/start nonce, accepts text on its existing Session callback and reports output, (c) document exact mode/version, ownership/quiescence mechanism, sequence/incarnation lifetime and capability gate. Timebox to these operations; missing atomic revision/create identity is a named blocker for writes, not a new research DAG.
- **Smallest checks:** real disposable source round trip for revision conflict and committed-write/lost-reply readback; actual controlled subprocess gets input without a second start. Synthetic JSON fixture covers malformed payloads but cannot substitute for source semantics.
- **Done:** selected mode and unsupported operations are explicit, each required source/session contract has a passing reproducible check or a specific implementation gap assigned to T03/T08. No “GasCity process fit” decision gate.

### T02. Add scoped source identity and explicit Issue links
- **Outcome:** workspace/project source bindings and standalone/linked work identities without changing Issue authority.
- **Dependencies:** T01 identity contract. **Priority/risk:** P0, medium. **Prerequisite:** source handle and admin authorization policy confirmed against existing middleware.
- **Targets:** new `server/internal/service/work_source.go`, `server/internal/handler/work_source.go`, `server/pkg/db/queries/work_source.sql`, additive `server/migrations/`; existing `project_resource.sql`, `handler/handler.go` dependency wiring and `server/cmd/server/router.go`.
- **Change/substeps:** (a) source/link records and concurrent indexes, (b) workspace/project validation and loaders, (c) link CRUD and source health/config APIs. Preserve existing Issue CRUD and loading paths.
- **Smallest checks:** DB-backed handler test creates a workspace source, project source and unchanged Issue, links/unlinks bead, denies another workspace and invalid project, verifies no synthetic Issue. Run migration/sqlc checks.
- **Done:** immutable scoped IDs survive source rename, link deletion does not delete either object, teardown explicitly handles new records without cascades.

### T03. Deliver source reads and repairable owner-routed writes
- **Outcome:** Beads list/detail/edit/create with visible receipts, no direct browser credentials.
- **Dependencies:** T01, T02. **Priority/risk:** P0, high. **Prerequisite:** real source revision/operation attribution gate for each enabled write.
- **Targets:** new `work_source_command` migration/query in source SQL; `service/work_source.go`, `handler/work_source.go`; concrete source command file in `server/internal/daemon/`, existing daemon client/heartbeat and `server/pkg/protocol/`.
- **Change/substeps:** (a) allowlisted source command and bounded parser, (b) command receipt and recovery state machine, (c) source revisions/list cursors/health and native/observe execution mode. No shell command concatenation or arbitrary paths.
- **Smallest checks:** selected source edits, concurrent revision conflict, outage, applied-but-reply-lost create/update, duplicate request hash mismatch and disabled source. Default tests use fake executable; must also pass disposable real-source contract from T01 before claiming write support.
- **Done:** a source-only task appears/edits without an Issue, write ambiguity is visible and repairable, an externally managed source refuses native Start.

### T04. Persist frozen graphs and unify queue admission
- **Outcome:** native A/B parallel and C join readiness survive restart.
- **Dependencies:** T02, T03 snapshot contract. **Priority/risk:** P0, high. **Prerequisite:** full targeted inventory of existing enqueue/claim/retry writers.
- **Targets:** new `server/internal/service/task_graph.go`, `server/pkg/db/queries/task_graph.sql`, `workflow_run` migration; existing `service/task.go`, `task_workflow.go`, `queries/agent.sql`, daemon task payload and `handler/daemon.go`.
- **Change/substeps:** (a) immutable bounded graph and queue graph IDs/indexes, (b) row-locked readiness/capacity/admission shared by all graph paths, (c) claim/start guards and graph task classification independent of Issue/chat/quick-create, (d) additive runtime capability gate.
- **Smallest checks:** DB concurrency test A and B admitted once within capacity, C blocked; cycles/cross-workspace nodes rejected; same-node concurrent enqueue unique, including different graph runs with overlapping bead identities; old daemon cannot claim graph work; legacy Issue/chat/quick-create claims unchanged.
- **Done:** one existing TaskService owns admission, queue attempts have preserved retry lineage, no null-shaped graph task is misclassified as quick-create, caller inventory is attached to implementation review.

### T05. Add run actions, fencing and restart/cancel recovery
- **Outcome:** Start/Hold/Resume/Cancel/Retry behave consistently through every entry point.
- **Dependencies:** T04. **Priority/risk:** P0, high. **Prerequisite:** lock-order and actual process-stop evidence defined in T01/T04.
- **Targets:** `service/task_graph.go`, `task.go`, `handler/task_lifecycle.go`, `handler/daemon.go`, new `handler/workflow_run.go`, `cmd/server/runtime_sweeper.go`, `daemon/daemon.go`, `daemon/workflow.go`, queue SQL.
- **Change/substeps:** (a) CAS/idempotent run actions, (b) graph-aware fail/retry/orphan/manual/autopilot/delegation paths, (c) incarnation fences on every mutation and local launch reservation, (d) cancel acknowledged-stop/unknown distinction. Retain legacy task policy on legacy paths.
- **Smallest checks:** race Hold/Cancel with queued claim and pre-start; stale claim/result/pin denied; duplicate Start doesn't Execute twice; daemon restart after possible launch blocks replacement until stop verified; automatic/manual retry contest yields one child; revocation denies new actions.
- **Done:** held/cancelling runs cannot acquire new starts, UI can distinguish stopping/unknown/stopped, retries cannot resurrect a stale controller or double-run a node.

### T06. Durable addressed mailbox and processed receipts
- **Outcome:** A can address B across offline/restart boundaries with thread and separate delivered/processed state.
- **Dependencies:** T04, T05. **Priority/risk:** P0, high. **Prerequisite:** current task-token/incarnation binding established.
- **Targets:** new `server/internal/service/task_mail.go`, `server/internal/handler/task_mail.go`, `server/pkg/db/queries/task_mail.sql`, `agent_mail` migrations; existing auth/workspace middleware helpers and router/protocol.
- **Change/substeps:** (a) direct mail row/send dedupe, (b) scoped inbox and recipient receipts, (c) retry redelivery/reply/thread rules and bounded retention. No broker and no transcript-as-mail substitution.
- **Smallest checks:** duplicate send same receipt, conflicting request body rejected, B offline/replaced gets same logical message with new fenced receipt, lost processed-ack response replays safely, spoofed sender/cross-run recipient denied.
- **Done:** delivered is visibly different from processed, old B cannot ack after replacement, processed state survives server restart.

### T07. Typed handoffs and atomic join-input selection
- **Outcome:** C can start only on A/B success plus valid required outputs and must acknowledge consuming selected inputs before success.
- **Dependencies:** T04-T06. **Priority/risk:** P0, medium-high. **Prerequisite:** v1 slots and artifact-reference allowlist agreed by default contract above.
- **Targets:** new `server/internal/service/task_handoff.go`, handler counterpart and SQL/migrations; existing `task_graph.go`, `task.go` completion transaction and task claim context builder.
- **Change/substeps:** (a) immutable attempt/slot handoff insert/validation, (b) atomic publish+success or required-output gate, (c) pin selected IDs in node state at admission and consumed ack API. Use existing run JSON instead of consumption table.
- **Smallest checks:** C blocked on missing/wrong-version handoff even if A reports success; retry A's stale output cannot replace selected input; duplicate consume ack safe; C success rejected until both consumed; artifact URL not fetched by server.
- **Done:** run detail can explain exact producer attempts and consumer receipts; no dependency requires C to acknowledge before its process can start.

### T08. Qualify and fix durable capture on the actual daemon process
- **Outcome:** output generated before attach and during network interruption remains replayable by incarnation.
- **Dependencies:** T01, T04, T05. **Priority/risk:** P0, high. **Prerequisite:** confirm producer seq/Session lifetime before index/backfill design.
- **Targets:** `daemon/daemon.go` batching and Session binding, new focused `daemon/task_message_spool.go`, `handler/daemon.go`, `queries/task_message.sql`, task-message migration, existing `handler/task_message_batch_test.go`, core task-message schema.
- **Change/substeps:** (a) failing lost-batch regression and local spool/ack lifecycle, (b) idempotent transactional server batch insert plus payload conflict detection, (c) incarnation-aware historical flush and scoped replay/fanout, (d) retention/error/gap metadata. Do not mutate ambiguous legacy history to manufacture uniqueness.
- **Smallest checks:** existing whole-batch/order/atomicity tests; source batch send fails then replays after restart; server commit/lost ack dedupes; truncated/corrupt spool surfaces error; retired incarnation cannot contaminate next attempt; disk-full/expiry explicit.
- **Done:** actual controlled-process output survives tested transport interruption, server acknowledgment means committed records, output-only graph tasks stream without synthetic Issue/chat.

### T09. Task-scoped same-session Web controls
- **Outcome:** authorized human text reaches the current graph worker, not a resumed or newly spawned process.
- **Dependencies:** T05, T08. **Priority/risk:** P0, high. **Prerequisite:** one selected Session.Steer implementation passes T01; unsupported adapters remain visibly unsupported.
- **Targets:** existing `handler/agent_workflow.go`, `daemon/workflow.go`, `service/task_workflow.go`, `queries/agent_workflow.sql`, `pkg/agent/interactions.go`, `pkg/protocol/`; queue controller fields and task routes.
- **Change/substeps:** (a) generalize chat-only workflow parent checks/registration to graph tasks using existing records, (b) controller lease with revocation and dispatch-time auth, (c) task session/steer endpoints, same request receipt semantics. No separate input process.
- **Smallest checks:** live PID/nonce unchanged after Web input; two controllers conflict; stale turn/incarnation rejected; member revoked after enqueue cannot dispatch; lost provider ack stays unknown without duplicate input; legacy chat control regression.
- **Done:** observer cannot type, browser reload doesn't Execute or Cancel, daemon restart reports the old session unavailable rather than pretending it survived.

### T10. Shared work/run/mail/session UI and validated API/event wiring
- **Outcome:** complete operator flows in Web and desktop using shared views.
- **Dependencies:** T02-T09 contracts. **Priority/risk:** P0, medium-high. **Prerequisite:** endpoint/event schemas stable and safe fallbacks defined.
- **Targets:** `packages/core/api/{client,schemas,schema,ws-client}.ts`, `types/events.ts`, `realtime/use-realtime-sync.ts`, `chat/queries.ts`; new `packages/core/work/queries.ts`, `packages/core/workflow/queries.ts`, `packages/views/work/`, `packages/views/workflow/`; existing transcript dialog/inline run components, project detail and shared navigation; new Web workspace `work` and `runs/[id]` routes, desktop `routes.tsx`, shared locales.
- **Change/substeps:** (a) schema/client/query/event contract commit, (b) explorer/detail/link/source health, (c) graph actions/mail/handoff/session/controller UI, (d) Web/desktop routing and translation/accessibility wiring. Do not define stores in views.
- **Smallest checks:** malformed HTTP/WS payload never mutates authority or crashes; unknown states disable controls; workspace switch isolates caches/cursors; component keyboard/pending/error test; desktop route smoke and Web operator route E2E. Review mobile response compatibility but do not claim root checks verify mobile.
- **Done:** user can complete every MVP flow without manually issuing API requests; desktop uses the same business views and guard/navigation seams; pending sends/input unknown remain visible.

### T11. First real Multica vertical-slice acceptance
- **Outcome:** integrated evidence for the first slice in section 7, not merely green fixtures.
- **Dependencies:** T01-T10. **Priority/risk:** P0 release gate, high. **Prerequisite:** authorized disposable managed environment and test-created workers, no account-backed agents.
- **Targets:** new focused `e2e/native-workflow.spec.ts` and TestApiClient helpers; canonical Go daemon/service/handler tests beside changed code. No independent imitation coordinator in tests.
- **Change/substeps:** (a) real PG + selected source + actual Multica server/daemon + controlled subprocess acceptance harness, (b) browser actions/tail/input/reconnect, (c) failure/restart/authorization scenarios and report, (d) broad regression commands.
- **Smallest checks:** section 7 exactly, plus `make test`, `pnpm typecheck`, `pnpm lint`, `pnpm test`, targeted `pnpm exec playwright test`; use `make check` where the environment supports combined validation. Inspect DB-test skips and executable guards. Run through the checkout's existing managed environment, never mix make/devenv managers.
- **Done:** recorded start counts/PIDs, queue attempts, mail receipts, selected/consumed handoffs and output cursors support all slice assertions. A mocked source or fake service alone cannot mark this task done.

### T12. Prove second-project federation and outage behavior
- **Outcome:** workspace-wide consolidation is true across two project/source contexts and workspace-only work.
- **Dependencies:** T11. **Priority/risk:** P1 required consolidation gate, medium. **Prerequisite:** independent second source identity, not a second label for the same store.
- **Targets:** source list/federation logic in `service/work_source.go`, core work queries, shared work explorer, focused E2E extension.
- **Change/substeps:** bounded fanout and per-source composite cursor, deterministic merge/tie-break by scoped identity, partial/stale counts and source outage states. Default page order uses a stable per-source snapshot where available, otherwise disclose live-list reset and avoid false snapshot guarantees.
- **Smallest checks:** same native bead ID in two sources remains distinct; second project filter and linked Issue identity correct; source outage doesn't erase healthy results or report failed source as zero; auth checked before source access/counts; cursor cannot be replayed with a different workspace/filter.
- **Done:** two projects plus workspace source and legacy Issues are navigable, source recovery reconciles without duplicate rows, consolidation claim allowed only now.

### T13. Implement preferred jj execution lifecycle
- **Outcome:** selected jj mode performs real Multica-owned repository work safely.
- **Dependencies:** T11, preferably T12 before broad rollout. **Priority/risk:** P1 required preferred-mode scope, high. **Prerequisite:** load jj skill/spec and qualify selected local jj version in a disposable repository during implementation.
- **Targets:** existing `daemon/execenv/local_worktree.go`, `execenv/git.go`, `repocache/cache.go`, `daemon/gc.go`, project-resource configuration and capability/schema/UI fields; new focused jj implementation only where existing execenv extension requires it.
- **Change/substeps:** explicit mode selection, jj workspace/change identity and isolation, dirty-user-state preservation, retries/resume/provenance, retain outcomes and safe cleanup. Keep existing Git mode. No destructive cleanup of unknown/user-owned work.
- **Smallest checks:** actual disposable jj repo, concurrent A/B workspaces, dirty/untracked user content preserved, restart/resume and cancellation, retained result discoverable, cleanup refuses foreign ownership. Fake-command tests supplement but do not prove jj semantics.
- **Done:** a graph actually runs through native jj preparation and result retention. Missing jj is a visible error, not Git fallback. First-slice no-repository qualification no longer stands in for this task.

### T14. Optional governed same-entity PG/Beads synchronization
- **Outcome:** selected linked Issues can intentionally synchronize shared fields in both directions without forced migration.
- **Dependencies:** T03, T12. **Priority/risk:** P2 opt-in, high. **Prerequisite:** chosen per-field contract and source CAS/readback gate. Default remains link-only if not enabled.
- **Targets:** source service/command receipts and link query; minimal sync metadata on association (base values, per-side revisions, mode, conflicts); existing Issue service event/outgoing-intent seam and shared link detail. No global replacement of IssueService.
- **Change/substeps:** (a) enable mapping and revision/base state, (b) durable local intent with Issue update plus reconciler, (c) conditional reverse apply/echo suppression/conflicts and deletion policy UI. PG collaboration-only fields and Beads dependencies remain owned separately.
- **Smallest checks:** one-sided edits propagate, simultaneous shared-field edits preserve both sides as conflict, out-of-order replay/echo suppressed, ambiguous apply repaired, no source disappearance causes delete, Issue completion distinct from graph success.
- **Done:** explicit per-link opt-in and visible repair/conflict state, existing unlinked Issues remain unchanged and Beads-only work still allowed.

### T15. GitLab bidirectional issue roadmap
- **Outcome:** deliver the previously requested GitLab issue breadth without delaying native swarm proof.
- **Dependencies:** T03, T12 and T14's field/receipt rules, not compulsory PG/Beads sync activation. **Priority/risk:** P2, high. **Prerequisite:** selected deployment contract and separately authorized credentials for later live GitLab acceptance.
- **Targets:** existing `server/internal/integrations/vcs/gitlab.go`, `server/internal/handler/vcs_webhook.go`, connection secret/UI paths, new focused `server/internal/integrations/gitlabissues/` and necessary mapping/inbox/outgoing-intent SQL.
- **Change/substeps:** (a) preserve MR/CI and add issue identity `(instance, numeric project ID, issue IID)`, authenticated durable webhook ingestion and lost-webhook reconciliation, (b) bidirectional supported fields/create attribution/conflict/429/echo repair, (c) confidential issue authorization, URL/redirect/egress checks and encrypted server-side credential handling. Never create a general connector framework solely for this task.
- **Smallest checks:** local `httptest` provider contract for duplicate events/outages/ambiguous create plus existing MR/CI regressions; separately authorized disposable live project covers supported two-way fields and distinct-role confidentiality. A service credential with broader access must not make confidential issues visible to every workspace member.
- **Done:** supported fields and unsupported comment/label/assignee cases are explicit, deployment-specific live validation is recorded or clearly blocked. Secrets and provider authority are not copied into agent mail/output.

### T16. Optional same-session terminal attach
- **Outcome:** optional tmux/herdr observation or control of the existing native execution, never a second CLI agent.
- **Dependencies:** T09, T11 and T13 when repository attachment is required. **Priority/risk:** P2 optional, medium-high. **Prerequisite:** selected attach mechanism must actually address the existing process. A process cannot be retroactively placed into a terminal supervisor by pretending a new process is the same session.
- **Targets:** `server/internal/daemon/workflow.go`, actual process launch/supervision and execenv paths, task session capability/schema and shared session UI.
- **Change/substeps:** qualify launch-time supervisor integration only where requested, expose same-incarnation attach metadata, mediate interactive input through the controller lease or ship observation-only attach. No provider abstraction or new execution owner.
- **Smallest checks:** actual terminal attach shows the same PID/start nonce/incarnation and start count, competing input loses the lease, browser and terminal observe the same output, reconnect does not launch another agent.
- **Done:** optional attachment is labeled unsupported until its actual same-session check passes. Web-first MVP remains independently deliverable.

## 7. First vertical slice and observable acceptance

**Actual owner:** current Multica server TaskService and daemon, not GasCity, not a test-only coordinator. Use one existing workspace, one project, one workspace source binding and one project binding to the selected disposable Beads/Dolt source arrangement. Include an unchanged legacy PG Issue, a standalone bead and an explicitly linked bead. A dedicated no-repository worker is permitted; repository execution is deliberately deferred to T13.

1. Seed with TestApiClient and the selected source fixture. Open the workspace work explorer. Verify exact scoped identity, project association, standalone and linked provenance. Edit a supported bead field through a durable receipt. Read the existing Issue through its unmodified API.
2. Preview and start frozen graph A/B -> C, capacity 2, two eligible agents/runtimes or a runtime whose actual capacity permits parallel processes. Record graph UUID, queue task IDs, claim generations, incarnation IDs and process start nonces. Verify A and B overlap in time; C has no queue/process start until both outcomes and required handoffs exist.
3. A sends B a durable direct message. Interrupt B's receipt response, reconnect/recover and retry the same receipt. Show one logical message and distinct delivered/processed timestamps. Restart receiver before processing in a separate scenario, verify redelivery to the new current attempt and rejection of the old receipt. Send a reply in the same thread.
4. A/B publish typed outputs and succeed. Verify C admission pins those exact output IDs. C fetches them and acknowledges consumption. A malformed/missing handoff holds C; stale A output after retry is rejected. C cannot settle successful before consumed acknowledgments.
5. Before opening Sessions, have the worker emit output. Then observe it through Web, acquire control and send text. Worker echoes input tagged with the same PID/start nonce/incarnation. Assert start count remains one for that attempt. Reload and reconnect browser, verify retained sequences including pre-attach output and no implicit Start/Cancel.
6. Fail a transcript report before commit and after commit/lost ack. Restore transport and restart daemon in separate scenarios. Verify spool recovery/dedupe, explicit gaps if capture could not persist, and no mixing of old/new incarnation output. Coordinator restart with live daemon preserves live execution rather than replacing it.
7. Hold while A/B active: no new C admission. Resume: C becomes eligible only when prerequisites actually hold. Race Cancel with claim/start and with a delayed successful result: no post-cancel successor, actual stop confirmed or visible unknown, stale input rejected. Retry only after known stop and within policy, with a new task/incarnation and preserved history.
8. Nonmember, cross-workspace member, revoked member on already-open socket, observer without invocation permission, stale controller and forged task sender cannot list/observe/control/ack outside authority. Project association never widens workspace access. Test source counts and error payloads for leakage too.
9. Legacy Issue CRUD/trigger/transcript and existing chat input regressions remain green. No synthetic Issue/chat created to make graph tasks work.

**First slice is not consolidation complete:** T12 must add a genuinely second project/source and outage recovery. **First slice is not repository-mode complete:** T13 must demonstrate real jj. **First slice is not GitLab/mobile delivery:** T15 and any separately scoped mobile work have their own gates.

## 8. Requirement-to-task and acceptance mapping

| Requirement | Tasks | Observable acceptance |
|---|---|---|
| Native Multica engine, no Organization | T02, T04, T05, T11 | Existing TaskService/daemon own queue and process IDs; only existing workspace/project tenancy. |
| Workspace/project explorer, standalone Beads and legacy Issues | T02, T03, T10, T11, T12 | Both kinds visible, links preserve identities, second-project federation/outage passes. |
| No forced canonical migration, owner-routed edits | T02, T03, T14 | Issue unchanged, standalone bead editable, optional sync visibly opt-in. |
| Persisted dependency/parallel/join readiness | T04, T05, T07, T11 | A/B overlap, C cannot start early, restart reconstructs identical frozen gates. |
| Durable addressed mail, threads, delivered/processed ack | T06, T10, T11 | One logical send, offline delivery, ack retry/reassignment and thread scope verified. |
| Typed handoffs and consumption | T07, T10, T11 | Required slots validated, admission pins exact outputs, C success requires consume receipts. |
| Attempts, fences, cancellation and recovery | T04, T05, T11 | Alternative admission paths cannot bypass policy, stale writes rejected, retry waits for stop. |
| Actual process capture, Web live tail/replay/input | T01, T08-T11 | Same PID/incarnation/start count on input, pre-attach output/reconnect and lost-ack recovery pass. |
| Operator start/hold/resume/cancel/retry | T05, T09-T11 | Server-confirmed actions, stopping/unknown explicit, no hidden process restart. |
| Scoped auth and untrusted agent content | T02-T11 | Nonmember/revoked/stale/forged-agent cases denied at HTTP, daemon dispatch and WS boundaries. |
| Existing API/schema/Query/WS compatibility | T08-T11 | Legacy responses/events preserved, malformed payloads cannot write caches/control state. |
| jj preferred, Git not banned | T13 | Real jj lifecycle passes; Git retained; selected jj never silently downgraded. |
| Same-session terminal attach optional later | T16 | Actual existing process and shared input controller, not a parallel CLI launch. |
| GitLab retained roadmap | T15 | Two-way supported issue fields, conflict/outage/confidentiality and MR/CI regressions. |
| Mobile scope honest | T10 compatibility review | Existing payloads remain compatible; no mobile swarm parity claim without independent mobile checks. |
| No new FKs/cascades, safe SQL generation | T02-T09 | Migration review confirms concurrent per-index files and explicit cleanup; `make sqlc` output included. |

## 9. Defaults and bounded unresolved decisions

1. **Source mode is the only external-store implementation gate:** start with pinned `bd` JSON/Dolt through a trusted daemon. T01 qualifies exact operations. Missing atomic edit/create semantics blocks those writes, not the choice of native execution owner. Do not substitute unverified CAS assumptions.
2. **Graph model:** immutable, one source snapshot per run for MVP, 128 nodes/512 edges/capacity 2. Cross-source graph dependencies are not needed to prove workspace federation. If later requested, snapshot each source explicitly rather than implying a cross-Dolt transaction.
3. **Mail:** direct run-node addressing, one recipient, at-least-once delivery and explicit processed ack. No general mailbox federation. Default maximum body 32 KiB, graph artifact reference list bounded by the typed schema. Tune only with evidence.
4. **Input:** semantic text steering for one qualified live adapter/controlled worker, existing request records, 30-second controller lease. Process pause, raw shell terminal injection and automatic unknown-input resend are out. An unsupported adapter does not satisfy the input requirement merely because transcript import exists.
5. **Execution ownership:** dedicated native-managed source first. Unknown external-controller liveness blocks adoption. Implement ownership transfer only when that workflow is actually requested and quiescence can be enforced.
6. **Retention:** output 7 days after terminal settlement by default, active/unacknowledged spool protected. Keep mail/handoffs/consumption and command dedupe through the run retention period. Final operational quotas/retention integration are selected at T08, not assumed infinite storage.
7. **Repository mode:** no-repository first slice, jj T13 next. Existing Git remains. Project/source configuration uses actual project resources and does not assume one repository per project.
8. **Sync:** link-only by default, per-link opt-in shared-field sync T14. GitLab is later requested scope with separate live authorization, not an omitted feature. No mobile feature requirements were specified beyond preserving existing behavior, so no new mobile UI task is invented.

These defaults are sufficient to start T01-T04 without asking the user to choose a new architecture. Findings that invalidate a safety contract become bounded implementation blockers, not an excuse to reopen broad comparative research.

## 10. Findings, evidence, validation and exclusions

### Findings

- The smallest native design is smaller than the prior report implied because durable workflow input requests, incarnation IDs, Session.Steer and lifecycle retirement already exist. Generalize chat-only gates rather than rebuild controls.
- The native queue has useful claim/retry fences but its null-shaped task classification and alternate retry paths require explicit graph integration. A new HTTP Start endpoint alone is insufficient.
- The producer batch-loss window and server lack of idempotent insert remain visible in current source. Standalone task live fanout also needs explicit widening with graph workspace authorization.
- A frozen bounded run JSON plus existing queue attempts can hold readiness and handoff consumption without a new generic orchestration engine or many graph tables.
- Source writes and process/input delivery have real ambiguity boundaries. Persist receipts and unknown states rather than promise exactly-once external effects.

### Evidence and validation performed for this design

Resolved authoritative baseline through waggle; read the baseline and historical review plus relevant prior-report sections. Read required repository/workflow/ponytail instructions and targeted specs. Selectively inspected current queue SQL, service lifecycle symbols, workflow request/session code, message batching/ingest, task-token middleware, schema/WS and route seams, project execenv and GitLab MR/CI surfaces. Verified the checkout HEAD and recorded baseline SHA-256. Historical review read has SHA-256 `c54f4f472d259d114ef35da8506fbb0b44f1f8b3d27eb0781be06f8c171021ff`, including its correction banner. Historical pre-banner handoff `3HZkVxpS` is not misrepresented as identical to these bytes.

Document review checked all six requested output groups against sections 1-9, task IDs/dependencies against the sixteen-task list, and current/new path distinctions against selective source reads. The resulting artifact is scratch-only and will be handed off as an immutable waggle snapshot. No machine-readable duplicate task scaffold is necessary.

### What I did not check

**No code tests, builds, browser acceptance, migrations, sqlc generation, DB operations, services, installed/account-backed agent CLIs, provider calls, secrets or account access were performed.** No source-runtime probes, real Beads/Dolt semantics qualification, jj execution, GitLab live validation, benchmark, source license audit, complete writer inventory, full mobile audit or clean-tree build. No claim that a fixture or historical test observation establishes current real-project acceptance. No repository files, Trellis state or old reports were changed. No additional workers or research DAG were spawned.

This plan is implementation-ready at the named seams, with T01's narrowly defined external-source/session qualification and later real-project gates still required. It is not an implemented integration and does not label any proposed acceptance test as already passed.
