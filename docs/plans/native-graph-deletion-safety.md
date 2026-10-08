# Native graph reservation deletion safety

Status: conservative deletion protection only. Graph Start and execution remain unavailable. This milestone does not implement graph-aware cleanup, restart reconciliation, typed handoffs or full swarm execution.

## Behavior

Source and workspace deletion return HTTP 409 while any graph-reserved task is reachable through the rows the deletion would sweep. This includes queued, running, failed-uncertain, failed-certain and completed-certain attempts. Even a terminal-certain attempt blocks until a graph-aware retention decision exists. Do not manually delete reservation evidence to bypass this guard.

The source guard checks the workspace-scoped source identity and references to its workflow runs. The workspace guard checks source, runtime, agent and workflow-run ownership independently with OR semantics. A cross-owned row cannot evade protection merely because its other owners are foreign. Unrelated foreign graph rows do not block deletion.

Checks run after the existing workspace/source locks and before destructive writes. A rejected transaction preserves reservation and parent rows. The workspace owner permission check remains the existing pre-transaction check, not a newly introduced authority-revalidation protocol. Future graph insertion must participate in these workspace/source locks. These tests do not prove arbitrary concurrent SQL writers safe.

Migration 605 adds a concurrent partial covering index for all graph-reserved rows, including terminal-certain history. The active-slot indexes 603/604 cannot cover this broader retention predicate. The new index excludes legacy history and is registered with interrupted-index cleanup in the migration runner. Its valid definition was observed on the managed PostgreSQL database. No production-scale latency or planner-choice claim is made.

## Requirement-to-check mapping

| Requirement | Check and observed result |
| --- | --- |
| Source deletion retains all reservation states | `TestGraphDeletionFenceBlocksWorkSourceDelete`: five-state matrix returns 409, whole task JSON and source/run/link rows preserved |
| Direct source and run-reference predicates work independently | The same test uses a foreign run for the source-only task, then an entirely foreign queue identity referencing the protected run; both return 409 |
| Workspace deletion retains all reservation states | `TestGraphDeletionFenceBlocksWorkspaceDelete`: five-state matrix returns 409 with task and parent preservation |
| Each workspace ownership arm is necessary | Independent source-only, runtime-only, agent-only and graph-run-only fixtures return 409 and preserve whole task rows |
| Permission denials still apply | Anonymous 401, plain source member 403 and workspace admin-not-owner 403 through production middleware |
| Graph-free deletion still works | Source/run/link cleanup and workspace/legacy-task cleanup return 204; unrelated foreign source/run rows survive |
| Post-lock snapshots see committed reservations | `TestGraphDeletionFenceSourceAfterLockWait` and `TestGraphDeletionFenceWorkspaceAfterLockWait`: exact `pg_blocking_pids` observation, holder commit, then 409 with inserted task and parents preserved |
| Existing operational contracts remain compatible | `TestDeleteWorkspaceRequiresOwner`, `TestWorkflowDraftParentLifecycle`, `TestWorkflowDraftDeletionLockOrderThroughRouter`, `TestWorkSourceCommandReceiptsThroughRouter` pass |
| Legacy task isolation remains intact | Existing graph fence matrix, six exact legacy controls, deferred polling and uncertain-capacity tests pass against real generated SQL |
| Migration and generated code remain reproducible | Concurrent up/down cleanup invariant tests and unchanged `make sqlc` output pass |

## Verified checkpoint

On October 8, 2026, exact source commit `9d370a027815dfe0c888a9f838b7e6a09dfa261b` passed immutable-archive acceptance. The four deletion tests and listed production-router compatibility tests passed with the race detector and three repetitions. The legacy queue checks passed with the race detector. There were no skipped tests in this acceptance run. Vet, full Go build and Darwin handler/service compilation passed. Cross-compilation is not runtime qualification.

Before the production fix, all four real HTTP deletion regressions returned destructive 204 instead of required 409, including both observed lock-wait cases. Earlier fixture/CGO failures were setup failures, not behavioral evidence. The corrected source-only test additionally removes an owned-run shortcut that could mask a missing source predicate.

Only reviewed migration 605 was applied for this increment. No browser, visual, frontend, mobile, complete repository suite or installed/account-backed agent CLI tests were run. This is a bounded protection increment, not complete native swarm acceptance.

## Operator checks

Use the checkout's existing managed environment and test database. Do not start another database manager or apply unreviewed pending migrations.

```sh
make sqlc
cd server
go test ./cmd/migrate -run 'TestEveryConcurrent' -count=1
go test -race ./cmd/server -run '^(TestGraphDeletionFence.*|TestDeleteWorkspaceRequiresOwner|TestWorkflowDraftParentLifecycle|TestWorkflowDraftDeletionLockOrderThroughRouter|TestWorkSourceCommandReceiptsThroughRouter)$' -count=3
go test -race ./internal/service -run '^(TestGraphTaskLegacyFenceMatrix|TestGraphFence.*Controls|TestGraphDeferredPollingIgnoresGraphRows|TestCountRunningTasksIncludesUncertainGraphReservation)$' -count=1
go vet ./cmd/migrate ./cmd/server ./internal/handler ./internal/service
go build ./...
```

Inspect output for integration skips. Do not attempt destructive deletion on a valued workspace to test this protection. No new visual control is delivered. Continue the existing Sources/enrollment and workspace/Issue/chat checklist in [the milestone guide](native-swarm-milestones.md). A future UI must display the server's conflict rather than hide it or fabricate successful cleanup.
