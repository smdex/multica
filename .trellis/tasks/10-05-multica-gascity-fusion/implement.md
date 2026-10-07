# Implementation checklist

User authorized implementation on October 7, 2026. Reviewed design.md is the accepted design/task breakdown. Preserve unrelated staged edits. Use existing devenv services only; no mixed manager, password changes or real-account agent runs.

## Dependency order

- [ ] T01 qualify pinned source mode and actual same-session worker input. Missing source CAS/create attribution keeps source writes read-only.
- [ ] T02 workspace/project source identities and Issue links.
- [ ] T03 source reads and repairable qualified writes.
- [ ] T04 frozen graph/queue admission shared by every graph execution path.
- [ ] T05 run actions, fencing, cancel and recovery.
- [ ] T06 durable direct mailbox and processed receipts.
- [ ] T07 typed handoffs and atomic join input selection.
- [ ] T08 durable producer capture/server idempotency/incarnation replay.
- [ ] T09 extend current live Session.Steer/control requests to graph tasks.
- [ ] T10 shared Web/desktop work/run/mail/session UI and validated API/event wiring.
- [ ] T11 real Multica vertical-slice acceptance and user-runnable setup.
- [ ] T12 genuine second-project federation, pagination and partial outages.
- [ ] T13 preferred real jj execution lifecycle.
- [ ] T14 optional selected per-link PG/Beads synchronization.
- [ ] T15 retained GitLab issue breadth with independently authorized live acceptance.
- [ ] T16 optional same-session terminal attach.

## Checks and review

Each task's file targets, regression cases and observed done conditions are specified in design.md sections6-8. Run narrow checks first via `devenv shell --`, record exact results and skips, then broaden backend and frontend checks when relevant. Exercise nonvisual production-router, CLI, daemon/process and PostgreSQL boundaries against the exact built milestone. The user's October 7 instruction assigns all browser/visual acceptance to them: do not run Playwright or browser-agent tests. Inspect DB skips. Root frontend checks do not verify mobile.

Independent code review must examine caller inventory, auth/claim/incarnation fences, crash windows, source write capabilities, malformed-response tests, migration/index rules and legacy regressions. Mint changed source snapshots with required symbols through waggle, resolve/read coverage, record accepted/rejected. Use td dependencies and review handoffs, do not self-approve substantive work.

## Final E2E delivery

Provide a reproducible setup script/runbook and actual application E2E using TestApiClient, current Multica PG/server/daemon, selected disposable source and controlled test-created subprocesses. Record A/B parallel start evidence, C join, mail receipts, handoff consumption, same-session input PID/start count, replay/reconnect/restart/cancel/unauthorized behavior. No imitation coordinator. Clearly distinguish source fixture tests from real Beads/Dolt and jj acceptance. Missing provider credentials block live provider checks only.

## Rollback/safety

Keep new capabilities explicit and additive. Legacy Issue/chat tasks retain current behavior. Unsupported source modes remain read-only, uncertain process starts block replacement. No destructive cleanup of unknown work; no mixing process/database managers; no whole-index commit that includes user staging.
