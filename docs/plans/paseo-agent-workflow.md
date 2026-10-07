# Paseo-like agent workflow contracts

Status: implementation contract, finalized 2026-09-19 after coordinator-supplied backend/UI explorer findings and local source review. This is a design, not a claim that the features already exist. Scope is existing web/desktop chats. No new chat route, transcript service, provider registry, or event bus.

## Provider contract (implement first)

Provider owners edit only their delegated files below. Preserve model discovery from JJ parent `ptsmpuxz`. Do not edit daemon, protocol, SQL, handler, or frontend files. Tests use fixture executables and temporary homes only.

Keep `Backend.Execute` unchanged. Add optional interfaces/types in `native_sessions.go` and `interactions.go`, plus the fields below in `agent.go`. `New(Config)` remains the provider constructor; no second registry. An unsupported adapter does not implement the native-history interface and leaves control callbacks nil.

```go
type NativeSessionProvider interface {
    ListNativeSessions(context.Context, NativeSessionListOptions) (NativeSessionPage, error)
    ReadNativeSession(context.Context, NativeSessionReadOptions) (NativeSessionSnapshot, error)
}

type NativeSessionListOptions struct {
    Cursor string
    Limit int // default 20, maximum 50
}
type NativeSessionReadOptions struct {
    Handle string // private provider locator returned by ListNativeSessions
    Revision string // exact listed snapshot; changed source fails, never silently refreshes
}
type NativeSessionSummary struct {
    NativeID string // stable native identity, separate from handle/path
    Handle string // daemon-private; never send to a browser
    Revision string
    Title string
    Cwd string
    Preview string
    UpdatedAt time.Time
    Model string // informational native model, not a selection override
}
type NativeSessionPage struct {
    Sessions []NativeSessionSummary
    NextCursor string
    Truncated bool // listing hit its scan budget
}
type NativeHistoryMessage struct {
    NativeID string
    Role string // only user or assistant
    Content string
    CreatedAt time.Time
    Events []Message // assistant timeline, existing text/thinking/tool types only
}
type NativeSessionSnapshot struct {
    Summary NativeSessionSummary
    ResumeSessionID string // existing ExecOptions.ResumeSessionID representation
    Messages []NativeHistoryMessage // selected active branch, oldest first
    Warnings []string // omitted non-text attachments/unsupported historical blocks
}

// Add to ExecOptions. Empty values preserve existing autonomous executions.
// InteractionMode: "" or "autonomous", or "chat".
// ResumePolicy: "" or "allow_fresh", or "require_native".
// No imported execution may clear require_native in a retry.
// InteractionMode string
// ResumePolicy string

// Add optional callbacks to Session:
// ControlState func() ControlState
// Steer func(context.Context, SteerRequest) (InputDelivery, error)
// RespondToInteraction func(context.Context, InteractionResponse) (InputDelivery, error)
// CancelPendingInputs func(context.Context) error

type ControlState struct {
    TurnID string // Codex foreground turn ID; Pi UUID for one prompt-to-agent_end cycle
    Active bool
    CanSteer bool
    CanApprove bool
    CanAnswer bool
}
type SteerRequest struct {
    ID string // stable daemon command UUID, deduplicated for this Session
    ExpectedTurnID string
    Content string // text only in v1, nonempty, <=32 KiB UTF-8
}
type InputDelivery struct {
    State string // accepted, rejected, unknown
    Code string // stable machine code; empty on accepted
}
type InteractionRequest struct {
    ID string // opaque normalized UUID; provider RPC ID stays private to adapter
    TurnID string
    Kind string // approval or question
    Title string
    Description string
    Tool string
    Input map[string]any // bounded display data, never trusted instructions
    Choices []InteractionChoice // approval only
    Questions []InteractionQuestion // question only
    ExpiresAt time.Time
}
type InteractionChoice struct {
    ID string // allow_once or deny in v1; no persistent/session-wide grants
    Label string
}
type InteractionQuestion struct {
    ID string
    Prompt string
    Options []InteractionOption
    Multiple bool
    AllowText bool
    Secret bool
}
type InteractionOption struct { ID, Label, Description string }
type InteractionAnswer struct {
    QuestionID string
    OptionIDs []string
    Text string
}
type InteractionResponse struct {
    ID string // stable response command UUID
    InteractionID string
    ExpectedTurnID string
    ChoiceID string // approval only
    Answers []InteractionAnswer // question only
    Cancelled bool
}
```

Use the existing `Session.Messages` stream for provider notifications: add message types `control-state` and `interaction`, with optional `Message.ControlState *ControlState` and `Message.Interaction *InteractionRequest`. They are control records, not text/tool transcript records. Emit a control state whenever the foreground turn starts/ends or availability changes. Daemon registration also reads `ControlState()` so it cannot miss the initial state. These callbacks must be safe against stop, provider exit, and turn completion. Provider scanners must continue reading while a human request waits; never block the JSON reader on a UI response.

All callbacks reject stale/nonforeground turns before writing. `Steer` rejects while an interaction is pending. No steering call invokes interrupt, new-turn, queue-next-task, or fresh-session fallback. A definitive provider refusal is `rejected`; a write with an uncertain acknowledgement is `unknown`. An error alone never proves nondelivery. Record command IDs before writing and return the same delivery outcome on duplicate calls; never repeat a write whose delivery is uncertain. `CancelPendingInputs` atomically closes admission, rejects waiting interactions, clears queued steering, and is idempotent. The daemon invokes it before the existing execution cancellation.

### Provider-specific requirements

| Provider | Native history and strict resume | Chat steering and interaction mode |
| --- | --- | --- |
| Pi | Read native JSONL under the effective Pi session roots; stable identity is header session ID plus root identity, locator is canonical file path. Read only the selected parent-linked active branch. Existing resume value remains the validated path. `require_native` must reject a missing file before `ensurePiSessionFile`; never create an empty replacement. | Chat execution uses `--mode rpc`, preserving existing event mapping; autonomous execution keeps its current one-shot JSON mode. Use RPC `prompt`, `steer`, `clear_queue`, `abort`, and `extension_ui_response`. Generate one turn UUID for the whole foreground agent cycle, not each tool/LLM iteration. Enable steering only with verified RPC and queue-cleanup support. Pi `confirm` becomes approval and `select`/`input`/`editor` become questions. Pi has no assumed universal tool-approval policy; advertise only interactions that extensions actually expose. |
| Codex | List with initialized app-server `thread/list` including pagination; hydrate selected `thread/read` with turns. Native identity/resume ID is thread ID. A failed `thread/resume`, missing returned ID, catalog retry, or daemon retry must never reach `thread/start` under `require_native`. | Use `turn/steer` with `threadId`, `expectedTurnId`, and text `input`; filter provider-native child threads. In `chat` mode explicitly set `approvalPolicy: "on-request"` on start/resume instead of inheriting an autonomous `never` setting. Handle command/file-change approvals, permission-profile requests, and `item/tool/requestUserInput` through the callbacks. No auto-accept in chat mode. Preserve current autonomous issue policy. Unknown approval/elicitation shapes fail closed with an explicit unsupported response. |
| Claude | List bounded top-level project JSONL files under effective `CLAUDE_CONFIG_DIR/projects`; exclude subagent transcripts. Native identity/resume ID is session UUID. Hydrate the selected active branch and preserve cwd. Never substitute a new conversation on strict-resume failure. | Steering unsupported in v1. Chat uses stream-json control requests with `--permission-mode default`, enables the stdin permission-prompt transport, and removes the autonomous `AskUserQuestion` disallow. Normalize `can_use_tool` and `AskUserQuestion`; responses map to their original control request IDs. Keep issue executions on existing bypass/disallow behavior. Enable only once a fake interactive handshake/response test proves the exact launch contract. |
| Others | Unsupported until their native contract is separately implemented and tested. | Current queue/stop remain. Missing callbacks mean unsupported, not an invitation to synthesize steering or auto-approve. |

Read-only history operations must not send a prompt, execute a tool, rewrite a native file, load project instructions as authority, or initialize a new conversation. Use runtime `Config` for executable/home/env; do not discover accounts or CLIs during default tests. Handles are private locators, not authorization: enforce root containment after canonicalization and again at file open. Snapshot limits are specified below. Return typed codes `unsupported`, `invalid_cursor`, `not_found`, `source_changed`, `session_busy`, `invalid_history`, `history_too_large`, `workdir_unavailable`, or `resume_unavailable`. Never fabricate complete history after a parse failure.

### Import ownership amendment

Native sources are never execution targets. Import creates a native fork/copy with a distinct owned identity, then hydrates that copy. The original remains unchanged. `ReadNativeSession` remains read-only; its source `ResumeSessionID` must never be scheduled. Add this optional interface without changing the reader/control names above:

```go
type NativeSessionImporter interface {
    PrepareNativeSession(context.Context, NativeSessionPrepareOptions) (PreparedNativeSession, error)
}
type NativeSessionPrepareOptions struct {
    ImportID string // stable request UUID, also the local preparation identity
    Handle string
    Revision string
    DestinationDir string // daemon-owned absolute directory; never browser-supplied
}
type PreparedNativeSession struct {
    Snapshot NativeSessionSnapshot // hydrate the immutable owned copy
    ResumeSessionID string // ONLY this identity may be scheduled
    ResumeCwd string
}
```

A provider may advertise browse while import is unsupported. `PrepareNativeSession` must prove a distinct resumable native identity, preserve native branch/context semantics, and send no user prompt. If a fork/copy cannot satisfy that contract, return `unsupported`; do not directly adopt the original or fabricate resumability. A small atomic manifest in `DestinationDir` maps `ImportID` to the prepared native identity and revision; it contains no parallel transcript and makes repeated preparation idempotent. Native files remain in the provider-required location. Detailed provider fork evidence appears below.

## Ownership for parallel implementation

| Owner | Exclusive writable scope | Depends on |
| --- | --- | --- |
| P1: Fermat | New `server/pkg/agent/native_sessions*.go` and provider history-specific files/tests only; readers and native fork/copy | Cannot edit `agent.go`, `codex.go`, `pi.go`, or `claude.go`; methods on their existing concrete backend types can live in new files |
| P2: Tesla | `server/pkg/agent/interactions*.go`, `agent.go`, existing Codex/Pi/Claude implementation and invocation tests; interactive hooks/policy and strict resume | P1 owns native history types; do not duplicate them |
| S: server | `server/internal/handler/`, `server/internal/daemonws/`, `server/pkg/protocol/`, `server/pkg/db/queries/`, `server/pkg/db/generated/`, `server/migrations/`, server router/wiring; any necessary strict-resume retry guard in `server/internal/service/` | Shared JSON below; owns all migration numbering and sqlc regeneration |
| D: daemon | `server/internal/daemon/` and tests, including task payload decoder/client, registry, heartbeat processing, native preparation manifest and report retry | P types; S protocol structs. Propose protocol changes to S, never edit those files concurrently |
| F: frontend | `packages/core/` and `packages/views/`, except Bacon files; shared import, controls, interactions and transcript rendering; existing app wiring only if required | S JSON; use `packages/core/api/schemas.ts` (plural) |
| Bacon: history | `packages/views/chat/components/chat-thread-list.tsx`, its tests and dedicated history-filter helper/tests | Existing session/agent data; no core, page, translation or route edits |
| Coordinator | This contract, integration conflict resolution, shared translation updates, cross-scope validation/E2E | Merge owner changes; arrange any service/app file exceptions explicitly |

No worker changes JJ ancestry, commits, parent model discovery, or ultragoal state. No mobile changes in these slices. Workers may define protocol structs/stubs first, but no capability is true until the full local path is implemented. Generated sqlc is S's output, never hand-edited.

## Public HTTP and JSON contract

All routes require authenticated human membership via `X-Workspace-ID`. Native history additionally requires the runtime's owning user, not merely membership, admin status, or the ability to invoke an agent. Task-scoped agent tokens cannot browse/import native history or approve themselves. Resolve runtime/session/task route parameters with existing loaders and use resolved UUIDs. Request body UUIDs use `parseUUIDOrBadRequest`.

`agent_id` identifies the Multica persona. `runtime_id` identifies its execution runtime. `provider` is the server-resolved protocol family (`pi`, `codex`, `claude`), never a trusted caller override. Import requires a nonarchived, invocable agent bound to that runtime. Recheck access at poll, dispatch, report/commit, send, and response; a runtime owner alone cannot write another user's chat.

| Method and path | Request | Response |
| --- | --- | --- |
| `GET /api/runtimes/{runtimeId}/agent-workflow-capabilities` | None | `WorkflowCapabilities` below |
| `POST /api/runtimes/{runtimeId}/native-sessions/list` | `{ "request_id": UUID, "cursor": null, "limit": 20 }` | `202 WorkflowRequest`, or `200` for replay of the same ID |
| `POST /api/runtimes/{runtimeId}/native-sessions/import` | `{ "request_id": UUID, "session_ref": string, "revision": string, "agent_id": UUID }` | `202 WorkflowRequest`; `200` if already imported/complete |
| `GET /api/runtimes/{runtimeId}/agent-workflow-requests/{requestId}` | None | `200 WorkflowRequest`, including terminal typed result |
| `GET /api/chat-sessions/{sessionId}/controls` | None | `200 ChatControls` |
| `POST /api/chat-sessions/{sessionId}/steer` | `{ "request_id": UUID, "task_id": UUID, "run_id": UUID, "turn_id": string, "content": string }` | `202 WorkflowRequest`; never enqueue an ordinary task |
| `GET /api/chat-sessions/{sessionId}/interactions` | None | `{ "items": Interaction[] }`, pending/resolving/unknown for current or unsettled run |
| `POST /api/chat-sessions/{sessionId}/interactions/{interactionId}/respond` | `{ "request_id": UUID, "task_id": UUID, "run_id": UUID, "turn_id": string, "response": InteractionResponseJSON }` | `202 WorkflowRequest`; `200` on an identical replay |

Steer/response requests return `runtime_id` in their envelope, so F polls the same runtime-scoped request URL. That poll authorizes the originating chat owner and current chat access; it does not apply native-history access rules to task controls. The operation kind determines the additional access check.

```json
{
  "runtime_id": "11111111-1111-4111-8111-111111111111",
  "provider": "codex",
  "online": true,
  "native_sessions": { "list": true, "import": true },
  "controls": { "steer": true, "approvals": true, "questions": true },
  "reason": null
}
```

This is `WorkflowCapabilities`. Unknown/missing capability fields are false. Offline retains readable history but disables new runtime operations. Static provider-name guesses do not enable controls. The runtime reports implemented/supported features; `ChatControls` adds current process/turn admission.

```json
{
  "id": "22222222-2222-4222-8222-222222222222",
  "runtime_id": "11111111-1111-4111-8111-111111111111",
  "kind": "native_session_list",
  "status": "completed",
  "result": {
    "sessions": [{
      "session_ref": "opaque-server-token",
      "revision": "opaque-native-revision",
      "provider": "codex",
      "title": "Investigate login flow",
      "cwd": "/work/project",
      "preview": "Investigate the login failure",
      "updated_at": "2026-09-19T10:00:00Z",
      "model": null,
      "imported_chat_session_id": null
    }],
    "next_cursor": null,
    "truncated": false
  },
  "error": null,
  "created_at": "2026-09-19T10:01:00Z",
  "updated_at": "2026-09-19T10:01:01Z"
}
```

`WorkflowRequest.kind` is `native_session_list | native_session_import | steer | interaction_response`; `status` is `pending | running | completed | failed | unknown`. `result` is null before completion and otherwise the operation's result. `error` is null or `{ "code": string, "message": string }`. Unknown status must render unavailable, never success. Import result is `{ "chat_session_id": UUID, "already_imported": boolean, "warnings": string[] }`. Controls result is `{ "delivery": "accepted" | "rejected" | "unknown", "message_id": UUID | null }`; only accepted steering has a message ID. Definitive rejection is `failed`; uncertainty is `unknown`. `accepted` means admitted by the live provider, not that the requested work finished.

The server issues `session_ref` as a random token mapped to the provider-private `NativeSessionSummary` in the persisted list request, scoped to workspace + importing user + runtime. It is not a path, base64 path, or caller-supplied native ID. References expire after 15 minutes; imports already accepted remain pollable after expiry. The browser never posts cwd, native handle, resume ID, home, executable, or provider overrides. Reuse of a request UUID with a different canonical body is `409 idempotency_conflict`; a UUID belonging to another scope is `404`. Responses include no raw provider protocol payload or resume handle.

```json
{
  "chat_session_id": "33333333-3333-4333-8333-333333333333",
  "runtime_id": "11111111-1111-4111-8111-111111111111",
  "task_id": "44444444-4444-4444-8444-444444444444",
  "run_id": "55555555-5555-4555-8555-555555555555",
  "turn_id": "turn-native-1",
  "active": true,
  "can_steer": true,
  "can_approve": true,
  "can_answer": true,
  "interaction_mode": "chat",
  "reason": null
}
```

This is `ChatControls`. Idle uses null task/run/turn IDs and false control booleans. `run_id` is a daemon-generated UUID for this `Execute` instance, registered with the server before admission; retries/restarts get a new one. The provider sees `turn_id`; the daemon/server additionally fence task and run. A later attempt or provider-native subagent cannot inherit controls from this record.

```json
{
  "id": "66666666-6666-4666-8666-666666666666",
  "chat_session_id": "33333333-3333-4333-8333-333333333333",
  "task_id": "44444444-4444-4444-8444-444444444444",
  "run_id": "55555555-5555-4555-8555-555555555555",
  "turn_id": "turn-native-1",
  "kind": "question",
  "status": "pending",
  "version": 1,
  "title": "Select a test scope",
  "description": "",
  "tool": "request_user_input",
  "input": {},
  "choices": [],
  "questions": [{
    "id": "scope",
    "prompt": "Which tests should run?",
    "options": [{ "id": "focused", "label": "Focused tests", "description": "Run tests for the changed package." }],
    "multiple": false,
    "allow_text": true,
    "secret": false
  }],
  "expires_at": "2026-09-19T10:16:00Z"
}
```

`Interaction.status` is `pending | resolving | resolved | expired | cancelled | unknown`. An approval uses kind `approval`, no questions, and choices `[{"id":"allow_once","label":"Allow once"},{"id":"deny","label":"Deny"}]`. Expose relevant bounded command/path/permission detail through `input`, without credentials. Resolve JSON is exactly one of `{ "choice_id": "allow_once" }`, `{ "choice_id": "deny" }`, `{ "answers": [{ "question_id": "scope", "option_ids": ["focused"], "text": "" }] }`, or `{ "cancelled": true }`. Validate option IDs/cardinality/text allowance against the stored request on both server and adapter. Never convert a question answer to a new chat prompt. Secret input support stays disabled until an explicit nonpersistent response path exists; unsupported secret questions fail closed.

HTTP errors preserve existing `{ "error": string }`, adding `{ "code": string }`. Required codes: `invalid_request` (400), `not_found` (404, including hidden foreign scope), `forbidden` (403 for known accessible scope without the required action), `unsupported` (422), `stale_turn`/`interaction_pending`/`already_resolved`/`source_changed`/`idempotency_conflict`/`session_busy`/`runtime_mismatch` (409), `history_too_large` (413), `runtime_offline` (503). Accepted operations report subsequent errors inside `WorkflowRequest`; never invent a replacement chat or a fallback queue send.

## Server/daemon transport

Extend the existing heartbeat pending-work pattern and WS event transport. Current generic daemon RPC is daemon-to-server; do not pretend it already supports server-to-daemon calls. S owns wire structs under `server/pkg/protocol/`, whose real files are `messages.go` and `events.go`.

Advertise/negotiate capability strings `native-session-import-v1` and `chat-controls-v1` in both daemon capability registration and heartbeat `server_capabilities`. Gate by both peers; absent support preserves current queue/stop behavior. Add runtime feature reporting to heartbeat request as optional `agent_workflow_capabilities` (the nested native_sessions/controls shape above). The server responds with optional `pending_agent_workflow: AgentWorkflowCommand[]`, maximum 10 commands. `daemon:pending_work` kind `agent_workflow` wakes the existing heartbeat path. HTTP heartbeat remains the transport fallback before delivery; no side-effect replay after ambiguous delivery.

`AgentWorkflowCommand` is `{ "id": UUID, "kind": WorkflowRequest.kind, "runtime_id": UUID, "workspace_id": UUID, "requester_id": UUID, "expires_at": RFC3339, "body": object }`. Body by kind:

- `native_session_list`: `{ "cursor": string|null, "limit": number }`.
- `native_session_import`: `{ "import_id": UUID, "chat_session_id": UUID, "agent_id": UUID, "handle": string, "revision": string, "native_id": string }`. `chat_session_id` is a server-preallocated destination reservation, and `agent_id` binds the imported clone to the selected persona. The server uses the stored list result, not user-authored locators. D chooses the owned destination directory.
- `steer`: `{ "chat_session_id": UUID, "task_id": UUID, "run_id": UUID, "turn_id": string, "content": string }`.
- `interaction_response`: same chat/task/run/turn tuple plus `{ "interaction_id": UUID, "response": InteractionResponseJSON }`.

S exposes daemon-authenticated reports:

| Endpoint | Body |
| --- | --- |
| `POST /api/daemon/runtimes/{runtimeId}/agent-workflow-requests/{requestId}/result` | `{ "status": "completed"|"failed"|"unknown", "result": object|null, "error": {"code":string,"message":string}|null }` |
| `POST /api/daemon/tasks/{taskId}/controls` | `{ "run_id": UUID, "turn_id": string|null, "active": boolean, "can_steer": boolean, "can_approve": boolean, "can_answer": boolean }` |
| `POST /api/daemon/tasks/{taskId}/interactions` | `{ "run_id": UUID, "interaction": InteractionRequestJSON }` using the provider request's snake_case fields |

Register `run_id` by adding it to the existing task-start body; controls/interaction reports cannot self-authorize a new execution. Existing task assignment/runtime access gates validate reports. D needs a process-local registry `task_id -> {run_id, Session, cancellation gate}`. Register controls before admitting commands; unregister and invalidate the run on terminal/stop. Never resolve a native thread by searching for the agent's latest task.

Daemon result shapes: list returns private summaries `{native_id,handle,revision,title,cwd,preview,updated_at,model}` plus `next_cursor,truncated`; S strips handles and issues public references. Import returns `{ "native_id": string, "owned_native_id": string, "provider": string, "resume_session_id": string, "work_dir": string, "messages": NativeHistoryMessageJSON[], "warnings": string[] }`; `native_id` is the source identity and `owned_native_id` is the distinct daemon clone. This is private and S transactionally converts it to the public import result. A native history message is `{ "native_id":string, "role":"user"|"assistant", "content":string, "created_at":RFC3339, "events": HistoricalEvent[] }`. Historical events use `{seq,type,tool?,content?,input?,output?,output_truncated?,created_at?}` with existing wire types `text|thinking|tool_use|tool_result|error`; daemon maps Go message names `tool-use`/`tool-result`. Control records and unresolved historical permission requests are never imported as live interactions. Control results report only `{delivery,code}`; S owns message creation and interaction status.

Provider control notifications must use a reliable path to these reports, not transcript `trySend` dropping. Until a pending interaction is acknowledged as persisted, D withholds the provider response. Retrying the same interaction report or result report is safe; retrying provider input is not. Existing ordered `task:message` reporting remains unchanged for text/thinking/tools.

S stores workflow commands in PostgreSQL so API replicas share dispatch/idempotency state. At heartbeat selection atomically move pending to running before emitting the command; never redispatch a running side-effect command after an uncertain disconnect. D remembers dispatch IDs for the live run and persists pending terminal reports using the existing terminal-report queue pattern. A lost acknowledgement can be retried as a report. A lost provider write result remains unknown, never automatically replayed. Read-only listing may use a new request ID; import recovery reuses `ImportID`/prepared manifest. Polls return stable records, not reconstructed guesses.

Deadlines: list 30 seconds, import 120 seconds, control admission 15 seconds, unanswered interaction 15 minutes. Distinguish pending-not-dispatched expiry (definite rejection) from dispatched expiry (unknown if side effects are possible). A late authenticated report can refine unknown to a definitive result, but cannot execute anything or overwrite a conflicting terminal outcome.

Polling is the required delivery contract; no new WS events are required for this release. F polls controls and unresolved interactions every 2 seconds while the chat is visible, plus refetch on reconnect/focus, and polls a pending operation every second (stop at terminal status). Future optional `task:controls`/`task:interaction` hints must use the existing authorized chat/user audience and monotonic interaction versions. Existing session-list invalidation publishes a completed import only after commit; its initiating client also invalidates on the completed operation. Existing `chat:message` publishes one accepted steering row. Keep `task:message`, `chat:done`, task backfill, and live-row reconciliation intact.

## Persistence and state transitions

S owns these schema additions. Use no new foreign keys; every added index, including indexes for new tables, gets its own single-statement `CREATE [UNIQUE] INDEX CONCURRENTLY` migration. Application deletion paths remove dependents transactionally. Use the existing workspace creation/deletion locks for import publication.

| Existing/new record | Required additions or fields |
| --- | --- |
| `chat_session` | `interaction_mode TEXT NOT NULL DEFAULT 'autonomous'`; `native_import_provider TEXT NULL`, `native_import_id TEXT NULL` (source identity), `native_import_revision TEXT NULL`, `native_imported_at TIMESTAMPTZ NULL`. Reuse existing `runtime_id`, `session_id`, `work_dir` for the owned execution copy. No new resume-pointer store. |
| `chat_message` | `imported_events JSONB NULL` (array of existing timeline event shapes), `native_message_id TEXT NULL`, `input_request_id UUID NULL` for canonical steering attribution. Existing `role`, `content`, cursor timestamp and UUID remain authoritative. |
| `agent_task_queue` | `interaction_mode TEXT NOT NULL DEFAULT 'autonomous'`, `resume_policy TEXT NOT NULL DEFAULT 'allow_fresh'`, `active_run_id UUID NULL`, `control_state JSONB NULL`, `control_updated_at TIMESTAMPTZ NULL`. Claim/start report carries immutable task policy plus the current run registration. |
| `agent_workflow_request` (new, bounded request ledger) | `id UUID NOT NULL` (unique/primary constraint installed using a separately built concurrent index), `workspace_id`, `requester_id`, `runtime_id`, nullable `chat_session_id/task_id/run_id/turn_id`, `kind`, `status`, `request_hash`, private `request JSONB`, private `result JSONB`, nullable `error JSONB`, `created_at/updated_at/expires_at`, nullable `dispatched_at`. This holds four operation types only; no arbitrary commands or transcript bus. |
| `task_interaction` (new) | `id UUID NOT NULL` (same index rule), `workspace_id`, `runtime_id`, `chat_session_id`, `task_id`, `run_id`, `turn_id`, `kind`, `status`, `version BIGINT`, `request JSONB`, nullable `response_request_id/response JSONB`, `created_at/updated_at/expires_at`. Native RPC response closures/IDs stay in the live provider session, not this table. |

Unique indexes: import identity `(workspace_id, creator_id, runtime_id, native_import_provider, native_import_id)` where source identity is nonnull; `chat_message(input_request_id)` where nonnull; imported message `(chat_session_id,native_message_id)` where nonnull. Source revision is deliberately not part of import identity: re-importing changed source history does not create/overwrite a second chat. Return the existing accessible chat; a different requested destination agent returns `409 already_imported` with no reassignment. Explicit deletion permits a later new import. Add bounded dispatch and unresolved-interaction lookup indexes. Retain operation identities at least while their chat/control run exists; purge expired list payloads after 15 minutes, and large import report payloads once their committed chat/messages exist. Do not retain a second full transcript in the request ledger.

Import transaction: recheck scope/runtime/agent binding and lock the workspace; validate the prepared snapshot; insert one explicit first-party chat with its owned resume pointer and source provenance; insert all historical messages/events; mark history read; mark operation completed; commit. Only then return/publish a chat ID. A unique-conflict retry returns the existing chat after authorization. No partial chat is visible, no task is enqueued, no `chat:done` is fabricated, and no usage/accounting is created for historical work. Failure rolls back all server-visible chat data. S acknowledges the report only after commit, so D retries the same result safely after a lost HTTP response.

Imported messages use original UTC times where valid and deterministic order on ties. Assign increasing UUIDv7 values for equal timestamps, or otherwise ensure the existing `(created_at,id)` cursor reproduces source order. Do not claim unknown times as native: use one deterministic import timestamp sequence and include a warning. Reject invalid ordering that cannot be faithfully normalized. Set `last_read_at` to at least the maximum imported timestamp so imports do not manufacture unread counts. Each assistant row's `imported_events` holds its associated existing timeline, including following tool results up to the next message boundary. `task_id` stays null: history is not a fictitious Multica execution. Imported events have local ascending `seq`, are settled, and never trigger live actions. Attachments not supported by this slice get a visible omission warning and no arbitrary URL fetch.

Steering transaction: validate creator/access + exact active task/run/turn + no pending interaction; insert the durable command. At provider-accepted report, atomically insert exactly one ordinary user `chat_message` with `input_request_id`, mark request completed, then publish that row. No assistant message or extra task is created. Provider echo is acknowledgement, not another user-message insertion. Rejection leaves no accepted message; unknown keeps a visible delivery-uncertain operation and preserves the text. Duplicate accepted reports find the same message. Do not downgrade a definitive accepted report to rejected merely because the turn subsequently ended.

Interaction response transaction: load and lock `task_interaction`; validate scope, exact live task/run/turn and response shape; compare-and-set `pending -> resolving`, increment version, save the chosen response and `response_request_id`, and insert its command atomically. One competing response wins. Replay of the winning request ID returns that operation; a different ID cannot resolve again. After provider acknowledgement, CAS the same resolving command to `resolved`; a definitive nondelivery may restore `pending` only while the exact run remains active and the request has not expired. Unknown delivery becomes `unknown` and cannot be retried automatically or with a different response. It remains visible with recovery copy until a definitive report or run termination. A browser disconnect never cancels a pending interaction.

Stop wins admission by closing D's run gate before cancelling the process, invoking `CancelPendingInputs` first. Pending-not-dispatched commands become rejected/cancelled; already dispatched commands preserve known accepted or unknown delivery. Pending interactions become cancelled, and no queued provider input leaks into a later run. Task completion, process loss, or daemon restart makes unresolvable pending interactions terminal; an old prompt is never restored into a fresh process. Transport reconnect to a still-running daemon preserves the same live callbacks and the database request. Existing task cancellation/queue behavior remains authoritative; steering does not drain or create ordinary queued tasks.

While an interaction is pending, the daemon watchdog treats it as waiting for a human, bounded by its explicit expiry, rather than agent inactivity. The transcript reader, heartbeat, cancel path and provider lifetime checks keep running. Expiry denies/cancels the provider request, never auto-allows. The expiry denial races through the same provider resolution gate as a human response.

## Chat policy, strict resume, and frontend wiring

Extend existing `POST /api/chat-sessions` and `PATCH /api/chat-sessions/{sessionId}` with optional `interaction_mode: "chat" | "autonomous"`; PATCH continues accepting exactly one editable field. Return `interaction_mode` additively on `ChatSession`. Creation without the field preserves autonomous behavior for existing clients. A capable new web/desktop client creates first-party chats with `chat`; opening an existing autonomous chat shows an explicit approval-mode choice. Changing mode is allowed only when no task for the chat is pending/running and the required daemon/provider capabilities exist. The UI must distinguish Pi extension prompts from general tool approval. No claim of tool approval for unsupported providers.

Import creates `interaction_mode: "chat"` and requires chat-mode execution support plus native import support. Source provenance is returned additively as `native_origin: {provider, imported_at}`; never expose native execution handles. Existing session mode controls tasks even if an older client sends the next message. The server snapshots mode into each task; D passes it into `ExecOptions`. Never infer interactive policy merely from nonnull `chat_session_id`: IM/channel chats, agent builders, background suggestions and issue tasks retain their existing autonomous policy unless explicitly opted in through this first-party flow. Custom args, runtime config or inherited permission flags cannot override an interactive task into bypass/never mode.

Every task of an imported chat has `resume_policy: "require_native"`. S pins it to the imported runtime; if the agent moves runtime, fail with `runtime_mismatch` and keep history readable. D validates owned resume identity and cwd before execution. All provider and daemon retry branches retain strict policy; missing state, unavailable cwd, resume rejection, transient resume busy, auth/network/catalog retry and process restart never clear the pointer or create a fresh conversation. Service retries may retry the same native identity if otherwise safe, but must not convert the job to a fresh-session task. An older daemon lacking strict import capability cannot claim these tasks. Ordinary non-imported task behavior is preserved.

F uses `packages/core/api/{client,schemas}.ts`, `packages/core/types/chat.ts`, and `packages/core/chat/{queries,mutations}.ts` for typed requests. New queries are keyed as `['workspaces', wsId, 'runtimes', runtimeId, 'agent-workflow-capabilities']`, `[..., 'agent-workflow-requests', requestId]`, and `['workspaces', wsId, 'chat-sessions', sessionId, 'controls'|'interactions']`. Integrate with existing key factories if their prefix differs, but retain all identity components. Use TanStack Query for these snapshots and mutation status; component state holds unsent form choices only. Every HTTP response passes zod + `parseWithFallback`; missing booleans are false, missing arrays empty, unknown enums unavailable. Never turn a malformed response into an enabled approval button or successful send.

Import lives in existing shared ChatPage/header/new-chat affordances outside Bacon's thread-list file. Explicitly selecting import opens a runtime selector restricted to owned runtimes and then fetches the small metadata page; do not enumerate local history when ordinary chat opens. Selecting a row and confirming the destination agent sends one import request, keeps progress visible, and navigates via the current navigation adapter to `/{ws}/chat?session={id}` only after completion. Show cwd and that the conversation is copied; continuing still operates in that working directory. Import does not create a project/issue, move files, copy a working tree, or grant new agent access.

The existing `ChatInput` keeps queue sending. When `ChatControls` admits an exact active turn, offer separate **Queue** and **Send now** actions. Disable Send now while an interaction waits, when control identity is incomplete, or when a command is pending/unknown. Do not silently change Send now into Queue on rejection. Stop uses the existing cancellation mutation. Surface accepted, failed and uncertain delivery in the composer; preserve text on failure, and require an explicit user decision after unknown delivery without claiming a retry is safe.

Render a pending interaction in a shared, accessible panel adjacent to the live turn. Approvals show action details and Allow once/Deny; questions render radio/check options and optional text according to their schema, with one submit/cancel response. Disable after resolving; reconnect repopulates it from GET. Questions use this panel, not quick-action pills. Keep the normal transcript usable while a prompt waits.

For imported assistant rows, pass `imported_events` into the same `buildTimeline`/`TimelineView` transformation the existing task messages use, in settled mode. Extract a shared pure event-shape adapter if needed; do not create a second renderer/cache or synthetic task IDs. Ordinary rows continue to use `taskMessagesOptions`, ordered `task:message`, and `task:{taskId}` live-to-persisted reconciliation. Preserve virtualized scroll position, text copying, long tool output expansion and markdown/code rendering. F owns no Bacon files. Bacon search/filter operates on existing permitted sessions, with distinct agent IDs even when providers match; retain existing pin/archive/unread semantics.

## Bounded native copy and execution invariants

- Discovery: at most 20 results by default/50 maximum; inspect metadata for at most 500 candidates and enumerate at most 5,000 directory entries/depth 4 per request. Cap each candidate's header/tail reads at 64 KiB/256 KiB and total scanned content at 8 MiB. Return `truncated: true` honestly; a cursor advances bounded enumeration, never means all history was loaded. Codex uses its native cursor and metadata-only `thread/list`; no per-row hydration. Omit malformed metadata candidates, counting them against the budget.
- Hydration: one selected session, at most 16 MiB native bytes, 1 MiB per JSONL record, 10,000 chat messages/50,000 events, and 24 MiB normalized report JSON. Set the matching HTTP body limit explicitly. Exceeding a bound rejects import, rather than silently presenting a complete but truncated conversation. Preserve native opaque context/compaction records in the execution copy even when they have no UI row.
- Source protection: canonicalize configured roots and selected paths; reject escapes, symlink components at open, nonregular files, path substitution and missing cwd. Use a rooted/no-follow file-open primitive, then validate the opened file's identity and before/after revision, not just a string prefix. Stage an immutable bounded copy while reading; if the revision or an in-progress native turn changes, fail `source_changed`/`session_busy`. No locking promise about unrelated native writers is needed because only the distinct copy is ever resumed. Source append after successful snapshot capture is harmless.
- Native ownership: `DestinationDir` is a daemon-owned 0700 directory with 0600 artifacts. Never delete or modify original history or cwd during import, rollback, archive, runtime GC or chat deletion. The copy's transcript identity belongs exclusively to this chat; acquire the existing execution/workdir guards before each run. An externally changed owned transcript fails closed instead of racing another writer. Store enough last-observed revision in its preparation manifest to distinguish Multica's own completed writes. Dedup native preparation by `ImportID`; clean only provably unreferenced owned artifacts after server confirmation, never guess following a lost acknowledgement.
- Reports: `requireDaemonRuntimeAccess` currently proves workspace access, not exclusive runtime identity by itself. Also bind request ID to its assigned runtime/registered daemon and validate the task's actual runtime plus active run. Never trust a report's `workspace_id`, provider, resume handle or task ID without matching the dispatched record. Register a new run only at the authenticated task-start transition; a controls report cannot replace another active run. Capability changes cannot resurrect stale commands.
- Import policy: new session UUID/path is essential; relabeling an original resume handle is not a copy. Verify the owned native snapshot corresponds exactly to the selected revision/active branch before publication. Do not ingest provider-native child-agent histories recursively. Do not load imported text into system prompts, extract instructions into AGENTS.md, or replay historical approval requests. Import is archival hydration; execution starts only on the user's later send.

## Provider-native copy evidence and stop conditions

Paseo supplies protocol evidence, not an architecture to transplant. Native-source code/docs below were read locally; no agent was executed.

| Provider | Concrete preparation contract | Local evidence and remaining implementation gate |
| --- | --- | --- |
| Pi | Capture a bounded source snapshot into the owned staging directory. Use native RPC `clone` against that staging copy with `--no-extensions`, then `get_state` to obtain a distinct ID/path. Keep the full active branch, unlike `fork` at a previous user prompt. Hydrate and resume only the clone. Linux and macOS use descriptor-relative `openat` with `O_NOFOLLOW`; other platforms leave Pi native browse/import unavailable until an equivalent primitive is implemented. | Installed Pi 0.85.1 `libexec/pi/docs/rpc.md` documents `clone`, `get_state`, `steer`, `clear_queue`, and extension UI; `docs/session-format.md` documents the tree format. Paseo `providers/pi/{session-descriptor,history-mapper,cli-runtime}.ts` confirms discovery and transport. `server/pkg/agent/pi_native_sessions_test.go` proves no prompt, no extension launch, stale-response drain, distinct identity, active-branch preservation, source immutability, and idempotent/crash-safe preparation using only a fake executable. Older runtimes without safe clone/copy support advertise import unsupported. |
| Codex | `thread/fork` with the selected `threadId`, `ephemeral:false`, `excludeTurns:false`; read the returned new thread with `thread/read`/`includeTurns:true`. Reject an unchanged ID or mismatched/changed source revision; never resume the original. The provider owns where the new thread's native files live. | Paseo `providers/codex/app-server-transport.ts:51` defines fork params; `providers/codex-app-server-agent.ts:2025` reads full turns and `:2032` forks. Its `:7128` lists import metadata. P1 must compare the selected source snapshot to the resulting clone and cover app-server cleanup/error responses. No prompt/tool request is allowed during preparation. |
| Claude | Browse the bounded native JSONL history only. `claudeBackend` does **not** implement `NativeSessionImporter`: a byte copy with the old session ID is insufficient, and no proven Multica CLI operation produces a fresh native session/message UUID chain without a turn. | Local Paseo dependency `node_modules/@anthropic-ai/claude-agent-sdk/sdk.d.ts:711` documents the no-turn `forkSession` operation, fresh UUIDs, chain preservation and resumability; `:1548` documents execution-time forking. This is evidence for the file contract, not evidence that Multica's existing CLI exposes a no-turn fork command. Do not add an SDK runtime dependency merely to import. |

Pi owned-directory correction: upstream [CLI session selection](https://raw.githubusercontent.com/badlogic/pi-mono/v0.85.1/packages/coding-agent/src/main.ts) forwards `--session-dir` (ahead of environment/settings) to `SessionManager.open`. [SessionManager](https://raw.githubusercontent.com/badlogic/pi-mono/v0.85.1/packages/coding-agent/src/core/session-manager.ts) otherwise defaults to the opened file's directory and writes clones there. Preparation therefore explicitly passes `--session-dir <canonical DestinationDir>` with the staged `--session` and `--no-extensions`. Source browsing keeps its original roots. `NativeOwnedSessionProvider.ReadOwnedNativeSession` accepts a separately persisted daemon-bound `DestinationDir` and `NativeID` for pre-execution checks and post-execution revision refresh; neither value may be derived from browser handles. The fixture writes outside source roots, rejects ordinary source reads of the owned path, and exercises label removal/rechaining, regenerated labels, and compaction reference rebasing while rejecting changed labels or opaque context. [Pi configuration](https://raw.githubusercontent.com/badlogic/pi-mono/v0.85.1/packages/coding-agent/src/config.ts) supplies the supported discovery precedence: session-dir environment override, agent-dir environment override plus `sessions`, then effective HOME's `.pi/agent/sessions`; tilde expansion uses effective HOME. No installed CLI is executed by these checks.

The existing local Pi package path used for this review is `/nix/store/9xzwcrqvcfpg9s3wc7k72w4mvri184sm-pi-0.85.1/libexec/pi/`. Treat it as inspection provenance, not a hard-coded application path. Native import is not accepted for a provider merely because browse works; its preparation and strict resume tests must pass.

## Acceptance slices and checks

| Slice | Acceptance boundary and canonical tests |
| --- | --- |
| 1. Native import, Pi then Codex/Claude | P1 fixtures: lightweight pagination never hydrates unselected files; active branches/compaction preserved; source unchanged; distinct resumable copy; malformed/truncated/oversized/path-escape/source-change cases fail. S DB tests: foreign workspace/user/runtime denied; concurrent imports/reports produce one complete readable chat; rollback publishes none; list token scope/expiry; re-import does not overwrite history. D fake execution: same owned identity after restart; strict resume failure never launches fresh. F tests and browser: choose runtime/session/agent, await import, reopen paginated history and send using the owned identity. |
| 2. Steering | P2 fake Codex/Pi tests assert exact native request and response mapping, stale turn and child-thread rejection, one write/one canonical message, duplicate response stability, unknown acknowledgement with no retry/interrupt, and queue cleanup before stop. D race tests cover task completion, stop, new run and pending approval during dispatch. F tests show Queue and Send now separately and preserve the draft/uncertain status. Normal queue remains unchanged. |
| 3. Approvals/questions | P2 launch tests prove Codex chat requests approvals, Claude enables control requests/AskUserQuestion, and issue tasks preserve autonomous flags; Pi exposes only supported extension interactions. S DB concurrency test proves one response wins and one dispatch occurs. D tests prove pending-human watchdog handling, timeout denial, reconnect reports and stale-run cleanup. F reload/reconnect recovers the actual pending request; options/text map to one native response and never to a new turn. |
| 4. Existing chat/history/rendering | Bacon tests distinguish agents sharing a provider and preserve pin/archive/unread ordering. F tests retain standalone `/{ws}/chat?session=` navigation, virtualized history, settled imported timelines, live `task:{taskId}` to `chat:done` reconciliation, code/tool text copy and scroll anchoring. Verify existing web and desktop adapter wiring; no new standalone route. |
| 5. Compatibility/integration | Missing/unknown capability or malformed JSON never enables control; old daemons keep queue/stop and cannot claim strict imported sessions. Fake daemons/providers drive browser E2E through real HTTP/DB persistence, not mocked-only UI. Two clients racing approval resolve once. Restart API/daemon and reopen the chat; history and owned resume identity remain, while dead-run prompts cannot execute. |

Workers run narrow fake-provider, handler/DB, schema and component tests first. S runs `make sqlc` after SQL changes. Coordinator runs managed environment checks (`make status`; use `.env.worktree`/managed `.env`, never an assumed database), appropriate `make test`, `pnpm typecheck`, `pnpm lint`, `pnpm test`, then focused `pnpm exec playwright test` with `TestApiClient`. Existing managed endpoints supplied by coordinator are API `http://localhost:18613` and web `http://localhost:13533`; check readiness before browser validation. Do not run real-agent account smoke tests. Fake-protocol/browser acceptance and unverified installed-provider execution must be reported separately.

Readiness requires each enabled provider's full vertical slice, not merely compiled optional interfaces or successful metadata listing. A provider whose native copy semantics remain unverified stays explicitly unsupported. No design acceptance here claims live provider/account validation.

## Source map and design validation

Multica: `server/pkg/agent/{agent,codex,pi,claude}.go`; `server/internal/handler/{chat,daemon,runtime_models}.go`; `server/internal/daemon/wsrpc.go`; `server/pkg/protocol/{messages,events}.go`; `server/pkg/db/queries/{chat,agent_task_queue}.sql`; `packages/core/api/schemas.ts`; `packages/core/chat/{queries,mutations}.ts`; `packages/core/realtime/use-realtime-sync.ts`; `packages/views/chat/components/chat-message-list.tsx`. These establish the existing runtime, pending-work, resume, transcript, schema and rendering boundaries. The generic WS RPC is daemon-to-server; new work is heartbeat-delivered.

Paseo root is `/home/smaximov/gh/paseo`; provider paths above are relative to `packages/server/src/server/agent/`. Also reviewed `provider-session-import.ts`, `providers/pi/agent.ts` and `providers/claude/agent.ts`: they demonstrate hydrate-before-publish, optional controls and provider-specific history, but their direct resume/import and fallback choices do not override Multica's source-preservation or strict-resume requirements.

Design-only validation: local contract/source inspection, JSON example parsing, referenced-file checks and focused whitespace/diff checks. Implementation tests and real-agent runs were not performed by this architecture lane. The artifact is the handoff; all unrelated worker changes and ultragoal state remain outside this lane.
