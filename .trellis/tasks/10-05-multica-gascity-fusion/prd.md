# Native Multica swarm implementation

## Outcome

Extend Multica's existing orchestration engine with dependency/parallel coordination, durable agent messaging and handoffs, same-session Web capture/tail/replay/input, safe cancellation/recovery, and displayed operational Beads/Dolt work. Workspace = City, Project = Rig. No Organization layer or replacement GasCity scheduler.

## Approved constraints

- October 7 user authorizes implementation, autonomous bounded decisions, Trellis/waggle/td and local code assistance, code review and a final user-runnable E2E setup.
- Preserve existing PG Issues/comments/projects and user changes. Beads-only tasks and governed opt-in PG/Beads sync are permitted. Default is independently owned entities with explicit links, no mandatory migration.
- Multica TaskService/queue/daemon owns native execution. External-owned work is observation-only until quiescence is proven. No competing admission path for the same task.
- jj is preferred. No-repository controlled workers qualify the first slice; genuine jj qualification is a separate required next increment. Existing Git behavior is not prohibited or silently substituted for selected jj mode.
- Default tests use controlled test-created executables and the checkout's declared devenv services. Never run account-backed installed agents, alter secrets/passwords, or mutate external provider projects without separate approval.
- Installed web/desktop/mobile clients retain existing API behavior. Shared features use core/views boundaries. New mobile screens are not in scope.

## Requirements and observable acceptance

| ID | Requirement | Acceptance |
|---|---|---|
| R1 | Workspace/project Beads explorer alongside PG Issues | Scoped workspace and project source work visible; stable IDs; linked/standalone distinction; two-project federation with explicit partial outages. |
| R2 | Repairable Beads reads/writes | Qualify real selected source mode; conditional supported edits, durable operation receipts/readback, conflicting edits and lost replies never silently duplicate create. Unsupported modes remain read-only. |
| R3 | Native frozen dependency workflows | A/B run concurrently under current TaskService; C waits for required successful attempts and typed handoffs. Queue/pre-start/retry paths obey one gate. |
| R4 | Durable scoped addressed mail | Offline recipient/replacement delivery, thread/reply identity, delivered versus processed receipts, safe duplicate requests and authenticated attempt identities. |
| R5 | Typed handoffs | Required output validated and persisted; successor admission pins inputs; consumption required before successor success, not before process start. |
| R6 | Web-first actual-session capture/control | Pre-attach output, live tail, durable replay/ack recovery, incarnation isolation, semantic input to the same process, authorized single controller. Reload does not start/kill an agent. |
| R7 | Fencing/cancel/recovery | Stale callbacks denied, Hold prevents new starts, cancel reports stopping/unknown/dead honestly, uncertain process liveness blocks replacement, restart preserves valid attempts. |
| R8 | Compatibility and security | Existing Issue/chat CRUD/control remain functional, source/task/mail/output access stays workspace scoped, membership revocation affects live subscriptions/control, HTTP/WS schemas validate consumed payloads. |
| R9 | Preferred jj execution | Real disposable colocated root, isolated jj workspaces, preserved dirty work, safe resume/result retention/cleanup and no silent selected-mode fallback. |
| R10 | Later selected sync/GitLab and optional attach | Explicit opt-in same-entity sync; supported two-way GitLab fields/conflict/outage/confidentiality and existing MR/CI regression gates; optional terminal attachment targets same session. |
| R11 | User-verifiable delivery | Reproducible managed setup, migrations, controlled-worker fixture, browser E2E command and manual checklist. Actual application/process/store evidence, recorded checks/skips and complete/partial/blocked mapping. |

## Definition of done

Each implemented scope maps to reproducible regression and real integration acceptance. Commit only implementation-owned changes. The final E2E setup must use real Multica server/daemon/PG and the selected source fixture with controlled workers, not an imitation coordinator. Research/design artifacts or unit-only checks cannot establish completed product delivery. Remaining unsupported source/provider/runtime capabilities are explicitly blocked or deferred, never fabricated passes.
