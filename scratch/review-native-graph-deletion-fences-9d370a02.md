# Read-only bounded review: native graph deletion fences @ 9d370a027815dfe0c888a9f838b7e6a09dfa261b

Verdict: **bounded approve** (corrected). No correctness gaps remain. One performance
shape observation, informational only. Root's acceptance run (sqlc identical, racecount3
DELETE + draft/receipt/owner routes, legacyqueuecontrols racecount1, vet/build, Darwin
compile, migration cleanup invariant, no skips) is accepted as reported and not re-run
(read-only mandate).

## Actual reads (no fabricated coverage)

Waggle tokens N1MadD6H/UMsyrVoi/C3iEaca8/XVroLkys/TZLYBgbU carry NO `--require`
contract (confirmed via `waggle coverage`: "no children and no contract"); they resolve
to plain file pointers. What was actually read, by file:

- `server/cmd/server/native_graph_deletion_routes_test.go` — full file (480 lines @
  9d370a02): fixtures, graphStatusMatrix (5 states incl. terminal-certain), source test
  (crossTask uses foreign fRun + byte-stable whole-row preservation), workspace test
  (guard task behind auth checks, four independent ownership legs), both
  AfterLockWait tests (pg_blocking_pids harness, FOR UPDATE / FOR KEY SHARE holders).
- `server/internal/handler/workspace.go` — fence insertion (qtx.ExistsGraphReservationForWorkspace)
  after LockWorkspaceForDelete, before any destructive write; ErrNativeGraphReservationConflict
  mapping in failWorkspaceDelete.
- `server/internal/handler/work_source.go` — error mapping arm.
- `server/internal/service/work_source.go` — DeleteWorkSourceCascade: fence after
  LockWorkspace + source FOR UPDATE, before DeleteWorkflowRunsForSource/links/DeleteWorkSource.
- `server/pkg/db/queries/task_graph.sql` + `generated/task_graph.sql.go` — both
  Exists* queries and comments.
- Cross-references: `pkg/db/queries/workspace_delete.sql` (LockWorkspaceTaskOwner*,
  ListTaskIDsByAgent|Issue|Runtime*), migrations 602/603/604/605, index inventory for
  agent_task_queue, `internal/service/task_graph_legacy_fence_test.go`, `cmd/migrate/main.go`
  registration.

## Correction: earlier finding retracted

My first report claimed the workspace fence missed an `issue_id` ownership arm (the
sweep deletes tasks via ListTaskIDsByIssue*). **Retracted.** Migration 602
(`agent_task_graph_identity_check`) requires graph rows to satisfy
`issue_id IS NULL AND chat_session_id IS NULL AND autopilot_run_id IS NULL AND
context->>'wakeup_id' IS NULL`. A row with non-NULL `graph_run_id` legally cannot
carry an issue_id, so the issue sweep arm can never match a graph reservation; an
`OR t.issue_id IN (...)` arm would be dead code. The current four-arm predicate
(source/agent/runtime/graph_run) exactly covers every ownership path a graph row can
legally occupy. Same conclusion for chat_session/autopilot paths, all NULL-guarded
by the same constraint.

## Seq-scan observation (informational, no planner latency claim)

Both Exists* predicates are OR-of-IN shapes; the repo's MUL-5999 note on
ListTaskIDsByAgentPage documents that this shape degrades to a full agent_task_queue
seq scan at scale. Bounded today: EXISTS short-circuits on first graph hit and runs
only inside delete flows under locks. Migration 605's partial covering index
`agent_task_graph_retention` (graph_run_id INCLUDE (work_source_id, agent_id,
runtime_id) WHERE graph_run_id IS NOT NULL) covers all legal graph rows and can serve
an index-only scan of graph history if the queries are ever rewritten to reference
the partial predicate. Worst case remains one scan over legacy history per delete
attempt. Report only; no claim that it currently misplans; no EXPLAIN was run
(read-only).

## Confirmed positives

- Fence placement post-lock/pre-destructive-write in both paths, pinned by lock-wait tests.
- Source fence arms match its cascade exactly (DeleteWorkflowRunsForSource + source scope).
- Migration 605: single-statement CREATE INDEX CONCURRENTLY, own file, registered,
  DROP INDEX CONCURRENTLY down; covers terminal-certain rows intentionally.
- Tests nonvacuous: byte-stable row preservation, foreign-row survival, graph-free
  legacy 204 positive controls, anonymous 401 / member and admin-not-owner 403 with
  a live reservation behind them, cross-owned leg isolation with foreign fRun.
- Workspace existing-owner check outside tx unchanged; no authority-recheck
  improvement claimed. All graph reservations incl. terminal-certain intentionally
  block until graph-aware cleanup. Future graph writers must participate in
  workspace/source locks — not yet implemented, acknowledged as ceiling.

## Uncertainty

Planner behavior unmeasured (no EXPLAIN, read-only). No claim about future
graph-writer protocol. Root's acceptance evidence taken on report.
