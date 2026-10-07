# Native implementation context

Read `prd.md` and `AGENTS.md` first. The full reviewed `design.md` remains authoritative for each task's field ownership, state machine, API and acceptance contract. Read the selected task's section before coding. This compact file prevents oversized automatic context injection, not a replacement design.

## Approved direction

- Workspace = City, Project = Rig. Multica has no Organization layer.
- Extend existing TaskService, PostgreSQL queue attempts, daemon and live Session callbacks. Do not add a second scheduler or adopt GasCity's daemon as the baseline.
- PG Issues/comments/projects remain canonical. Standalone Beads tasks remain source-owned and may link explicitly to Issues without creating a synthetic Issue. Optional same-entity sync is explicit, governed and requires qualified conditional writes.
- Register approved opaque source handles against a configured runtime. Browser configuration must not select arbitrary executables/paths or carry source secrets. Observe sources cannot start native execution.
- Source fields/dependencies and PG controls/projections do not share an atomic transaction. Never hide ambiguous source writes or silently dual-write.
- Prefer jj for repository execution. Do not falsely claim a controlled no-repository worker proves jj support.

## Reuse and safety

Reuse `active_run_id`, durable `agent_workflow_request`, Session.Steer and lifecycle retirement. Extend chat-only gates for graph tasks rather than replacing controls. Keep queue retry lineage and task-token identity.

Frozen bounded graphs admit work through one TaskService gate. Reserve graph-node and source/native active slots, including uncertain process liveness. No new start after Hold/Cancel. Unknown liveness blocks replacement until stop is proved. Different graph roots sharing a bead cannot run it concurrently.

Mail is durable addressed state, not transcript text. Delivered and processed are separate, attempt-fenced receipts. Typed immutable handoffs pin join inputs atomically, and required consumption gates success.

Capture retains immutable batches until a definite server receipt. Capability discovery prevents unsafe retry to legacy servers. A batch UUID alone does not prove incarnation authorization or restart replay. Keep compatibility for installed clients, validate HTTP and WebSocket payloads before Query writes, and route workspace-scoped keys/events correctly.

## Dependency-ordered tasks

| Task | Scope | Dependencies |
| --- | --- | --- |
| T01 | Selected Beads source operations and actual same-process Session qualification | None |
| T02 | Scoped sources, project bindings and explicit Issue links | T01 identity |
| T03 | Source reads and qualified owner-routed writes/receipts | T01, T02 |
| T04 | Frozen graph persistence and all-caller queue admission | T02, T03 snapshot |
| T05 | Start/Hold/Resume/Cancel/Retry fences and restart recovery | T04 |
| T06 | Durable addressed mail and processed receipts | T04, T05 |
| T07 | Typed handoffs and atomic join input selection/consumption | T04-T06 |
| T08 | Producer capture, server dedup, incarnation and replay | T01, T04, T05 |
| T09 | Graph task live Session controls via current workflow requests | T05, T08 |
| T10 | Shared Web/desktop work/run/mail/session UI, schemas/events | T02-T09 |
| T11 | Real native vertical-slice E2E and runnable user setup | T01-T10 |
| T12 | Second-project federation, pagination, partial outages | T11 |
| T13 | Real preferred jj execution lifecycle | T11, preferably T12 before broad rollout |
| T14 | Optional selected per-link PG/Beads sync | T03, T12 |
| T15 | Retained GitLab breadth with separately authorized live acceptance | T03, T12, T14 field/receipt rules without required sync activation |
| T16 | Optional same-session terminal attach | T09, T11, and T13 for repository attachment |

P0 complete slice is T01-T11, not one commit. P1 includes consolidation and jj. P2 retains optional sync/GitLab/attach. Unsupported source operations remain visibly unavailable with assigned limitations, never successful mocks.

## Verification and handoff

Use this checkout's existing devenv services only. Run narrow checks through `devenv shell --`, inspect actual tests and DB skips, then broaden by risk. Current user check/runbook: `scripts/check-native-swarm-foundation.sh` and `verification.md`. These foundation checks are not full swarm acceptance.

Default tests use explicit test-created fake/missing executable paths. Actual qualification uses disposable scratch sources/processes, never user repositories or account-backed agents. E2E setup/teardown uses TestApiClient. Full acceptance must observe actual native coordination, A/B parallelism, C join, mail/handoffs, same-session PID/start-count input, live/reconnect output, restart/cancel/unauthorized behavior, and real source/jj boundaries. Existing fake workflow-daemon browser tests do not replace this.

Track with td, use independent review rather than self-approval, and transfer source through waggle snapshot/required symbols, coverage and accepted/rejected records. Commit only owned paths because unrelated user staging exists. Preserve scratch artifacts. Do not reset credentials, mix database managers or perform destructive cleanup of unknown work.
