# Native swarm milestones

## Milestone 1: verified foundations

This milestone is not a complete swarm engine. It includes durable task-message batch receipts, retry-safe producer capture, same-process Session input/output qualification, a read-only Beads adapter, workspace/project source bindings, and many-to-many Issue/source-item links. Source-only explorer UI, owner-routed command execution, graph scheduling, mail/handoffs, graph session controls, restart recovery, and the preferred jj execution lifecycle are not delivered yet.

The link regression failed against the old PostgreSQL index, then passed after migrations 592-593. Source lifecycle, authorization, teardown, concurrent-index cleanup and the nonvisual foundation script passed. Beads writes remain disabled because revision CAS and atomic create attribution are not qualified. Default tests do not invoke installed agent CLIs or provider accounts.

## Milestone 2: authenticated read-command receipts

The read-only receipt API now supports stable request UUIDs, exact owning-daemon/runtime claims, canonical typed results, replay conflict detection, source-enabled/configuration fences, five-minute deadlines, bounded expiry sweeping, and source/workspace cleanup. The nonvisual foundation script includes the production HTTP router and actual JWT/daemon-token middleware, PostgreSQL lock-order regressions and malformed-result checks. Existing Issue/comment/workspace HTTP behavior also passed.

| Interface | Contract |
| --- | --- |
| `POST /api/work-sources/{sourceID}/commands` | Workspace owner/admin submits `request_id` UUID and `command: "list"` with optional `limit` up to 200, or `command: "read"` with opaque `native_id`. Reuse the UUID only for retries of the same request. |
| `GET /api/work-sources/{sourceID}/commands` | Workspace member reads bounded receipt history without result bodies. |
| `GET /api/work-source-commands/{commandID}` | Workspace member reads the complete scoped receipt. |
| Daemon claim/result routes | Require an actual `mdt_` credential matching the bound daemon, not a user JWT/PAT or a claimed daemon ID header. |

**Automatic source reads are not delivered yet.** Pending discovery is available through `GET /api/daemon/runtimes/{runtimeId}/work-source-commands`, with exact daemon identity and current runtime/source/revision/deadline fences. Discovery does not claim or execute a command. Credential bootstrap and automatic dispatch remain next steps, so unclaimed reads expire to a visible failed receipt. The HTTP regression uses a test-owned daemon credential and typed test result, not a real source executor. Observe-mode sources cannot start agents or write Beads data. There is still no new source explorer or swarm UI to visually test.

## Milestone 3: local read bindings

The CLI accepts an operator-local `work_source_reads` binding list. Each entry pins a workspace UUID and exact source handle to an explicit absolute Beads directory and executable path. It is profile-scoped and never sent to the server. Unknown fields, duplicate bindings, relative paths, `null`, and trailing JSON are rejected before saving. Empty input or `[]` clears the list.

```bash
multica --profile dev config set work_source_reads '[{"workspace_id":"<workspace UUID>","source_handle":"<exact source handle>","beads_dir":"/absolute/path/to/.beads","executable":"/absolute/path/to/bd"}]'
multica --profile dev config show
multica --profile dev config set work_source_reads ''
```

Replace the placeholders with your actual binding. Use the existing `--profile` flag for profile isolation, not `MULTICA_PROFILE`. `config show` displays workspace/handle identities without printing executable or directory paths.

The daemon-side read helper uses only these local bindings, permits list/read, runs the approved executable without a shell or PATH lookup, and bounds typed results. It is not connected to a polling loop yet. These settings do not start a process, activate automatic reads, or add UI controls. Tests use test-created executables, not your installed Beads CLI or agent accounts.

## Milestone 4: registration ownership and atomicity

Runtime registration preserves the existing owner. A different member or admin cannot adopt another owner's runtime or an ownerless runtime, including custom and failed profiles. Daemon-token reconnects must match the exact authenticated daemon and cannot request legacy identity merges. Same-owner reconnects, profile identity, custom names and source-free legacy merges remain supported.

A registration request commits all its runtime changes together. Membership removal serializes with registration, so a request waiting behind a successful removal cannot create another runtime afterward. Notifications are emitted only after commit.

A legacy runtime with a work-source binding blocks migration with HTTP 409. The entire registration rolls back, leaving its runtime, source and agents unchanged. Reconnect using the existing approved identity. Do not delete or reassign bindings to force migration without an explicit reviewed migration workflow.

Run the actual router and PostgreSQL checks in your managed testing checkout:

```bash
devenv shell -- bash -c 'cd server && go test -race ./cmd/server -run "^TestDaemonRegistration.*ThroughRouter$" -count=3 -v'
devenv shell -- bash -c 'cd server && go test -race ./internal/handler -run "^(TestDaemonRegister.*|TestDaemonRegistrationUpsertOwnerGuards|TestRuntimeProfileDeleteLockSerializesRegistration|TestWorkSourceRuntimeMergeRefusal)$" -count=1 -v'
```

These tests use test-created credentials and inert runtime providers, not installed agents. This milestone adds no migration or UI. Credential bootstrap is a separate slice below. Automatic source-read delivery remains unimplemented.

## Milestone 5: scoped source-read credentials

An owned online runtime can exchange a fresh human JWT or PAT at `POST /api/daemon/runtimes/{runtimeId}/source-read-token` with `{"scope":"source:read"}`. The returned `msr_` bearer expires within 120 seconds, clipped to its parent expiry, and is never a user credential or a general daemon token. An admin cannot exchange for another owner's runtime. Cookies, ownerless runtimes and offline runtimes do not qualify.

The capability permits only that runtime's pending-read GET and command claim/result POST routes. Authorization rechecks current ownership, exact daemon identity, the membership row's UUID and any parent PAT in the actual receipt transaction. PAT revocation and membership removal invalidate it. Re-adding membership creates a new incarnation and does not revive the old capability. Ordinary JWT logout is not a server-side revocation mechanism for an already copied JWT.

Run the nonvisual route checks through the checkout's managed database:

```bash
devenv shell -- bash -c 'cd server && go test -race ./internal/auth ./internal/middleware -run SourceRead -count=1'
devenv shell -- bash -c 'cd server && go test -race ./cmd/server -run "^TestSourceReadToken" -count=3 -v'
```

These checks use real HTTP routes and PostgreSQL with test-created credentials. Do not paste returned credentials into logs or screenshots. This slice introduces no migration, token registry, source-write permission or UI. It enables authenticated delivery interfaces, not automatic daemon execution. The local bindings and executor still await polling integration.

## Milestone 6: automatic read-only delivery

This milestone connects the local bindings to the actual daemon lifecycle. It supersedes the earlier milestones' statements that polling integration is missing. An unconfigured daemon performs no source-read HTTP calls or subprocess launches. A configured daemon discovers pending commands only in bound workspaces, claims only exact workspace/handle matches, exchanges short-lived runtime-scoped credentials, and runs the explicitly approved Beads executable. Observe sources still cannot start agents or write source data.

The nonvisual acceptance gate now exercises the production router, PostgreSQL and `Daemon.Run`: list and detail results, a failed source with a generic diagnostic, cancellation/shutdown, and a lost reply after the server commits a report. That last case must retry the identical outcome without a second subprocess. Unit checks additionally cover malformed receipts, expired or divergent claims, unbound handles/workspaces, second-runtime discovery, credential refresh, rate limits and deadline-bounded reporting. Test executables are disposable fixtures, not installed agents or provider accounts.

For an operator-owned manual check:

1. Use the published build in a disposable testing workspace. Authenticate the CLI as the owner of the online runtime. Starting the daemon can also execute ordinary queued agent runs and probe configured agent CLIs, so use only a workspace and executables you intentionally approve.
2. Create or identify an observe work source through `POST /api/work-sources` with `runtime_id`, `name` and `source_handle`, and optional `project_id`. Use `X-Workspace-ID` with the actual workspace UUID. Keep its runtime identity unchanged.
3. Save the explicit local binding with the milestone 3 command, then start the selected profile with `multica --profile dev daemon start --foreground`. If already running, restart that profile after saving settings. Bindings load at startup, not dynamically.
4. As workspace owner/admin, submit `POST /api/work-sources/{sourceID}/commands` with a fresh UUID `request_id` and either `{"command":"list","limit":2}` or `{"command":"read","native_id":"<existing native ID>"}`. Include `request_id` in the same object. Retry only the identical object with the same UUID.
5. Poll `GET /api/work-source-commands/{commandID}` as a workspace member. Expect `pending` then `claimed` then `succeeded`, with `result` a JSON-encoded string containing a list array or a detail object. Invalid source availability yields `failed` with a generic diagnostic, not local paths or executable stderr. Never copy capability tokens into screenshots or logs.

Polling is serial, one command per runtime per round. Slow sources and unavailable runtimes can delay other targets. Execution is bounded to 30 seconds and the command deadline. Reports are retained only in memory: same-process lost-reply retry is delivered, but a daemon restart loses an unreported outcome and leaves the claimed command to server expiry. Durable restart replay, graph scheduling, mail/handoffs, source explorer/session controls and preferred jj execution remain separate unfinished work. There is no new visual source or swarm page to test yet.

## Milestone 7: shared read-only Sources explorer

Web and desktop share a workspace `Sources` page at `/{workspaceSlug}/sources`, with the normal workspace guard and navigation adapters. It is a read-only inspection slice, not a graph or execution control surface. Opening the page or selecting a source creates no command. Owners/admins can explicitly refresh the first 50 items or read one item. Ordinary members can inspect existing receipt history without invoking the daemon. Disabled sources retain history but cannot accept new reads.

Each explicit read owns a fresh request UUID. Transport-loss Retry reuses the identical body and UUID. Server receipts/results remain in workspace-scoped TanStack Query caches, with exact source/request/command identity checked before use. Pending, claimed and unknown statuses block another request. A passed deadline is not proof of failure: automatic polling stops and Recheck asks the server for the actual outcome. Unknown newer-backend status strings remain unknown rather than becoming a fabricated failure or success. Source, workspace and configuration changes reset only local pointers/intents, not server authority.

List results are bounded to the first 50, not an exhaustive source snapshot. Detail revisions are opaque item revisions, not dependency topology CAS. The explorer adds no source writes, native Start, synthetic Issue, graph, mail or session controller. It does not establish two-project federation or restart replay.

For nonvisual public HTTP/TypeScript contract acceptance in a managed testing checkout:

```bash
devenv shell -- bash -c 'cd server && MULTICA_RUN_SOURCE_CLIENT_CONTRACT=1 MULTICA_SOURCE_CLIENT_PNPM="$(command -v pnpm)" go test -race ./cmd/server -run "^TestSourceReadFrontendContract$" -count=1 -v'
```

This gated test prepares receipts through the actual production router and PostgreSQL, then runs the checked-out TypeScript client/decoder against that server. It does not run a browser, discover installed agents, start services or apply migrations. Default frontend tests skip this integration file unless the Go fixture supplies its explicit environment.

## Milestone 8: qualified dependency observations

Published code `2adbae9b4c1bd5bd74f7c472a21242fdc341b284` adds typed outgoing dependency observations to detail receipts. The qualified Beads adapter reads metadata and then the strict raw dependency operation described in [qualified-dependency-reads.md](qualified-dependency-reads.md). An explicit complete empty observation serializes as `dependencies: []`. Legacy receipts remain readable, but omitted completeness is never promoted to complete. Unknown dependency kinds and external IDs are retained as observations, not interpreted as runnable producers.

These reads are not an atomic metadata/topology or cross-item snapshot. An item revision does not fence dependency edits. The existing Sources page remains an item explorer, not a dependency graph preview or Start control. No source writes or graph execution are enabled.

The exact exported candidate passed default Beads, CLI/config, source service/handler and daemon read checks under the race detector. Production-router/PostgreSQL command, token, registration and daemon dispatch suites passed three iterations, including terminal reply loss without reexecution, canonical typed edges, stale/foreign credentials and expiry after lock waits. Explicit opt-in checks additionally exercised the approved actual Beads executable and actual daemon lifecycle against the production router and PostgreSQL three times. Tests use disposable source fixtures and inert test-created agent executables, never provider-backed agents. Both server and CLI built and CLI help ran. See the qualification document for explicit opt-in setup, not an installed CLI auto-discovery step.

Broader acceptance exposed an existing expiry selection defect: old orphaned receipts occupied every bounded selection slot. The shared SQL selector now excludes receipts whose exact workspace/source no longer exists. A failing orphan regression passed after the fix, as did ten repeated existing expiry checks and the corrected immutable acceptance suite. No existing database data was deleted and no migration was added.

To exercise the default nonvisual paths in a managed testing checkout:

```bash
devenv shell -- bash -c 'cd server && go test -race ./pkg/beads ./internal/cli ./cmd/multica -count=1'
devenv shell -- bash -c 'cd server && go test -race ./internal/daemon -run TestWorkSourceRead -count=1'
devenv shell -- bash -c 'cd server && go test -race ./cmd/server -run "^Test(SourceReadDependencyReceiptsThroughRouter|SourceReadDaemonDispatchThroughProductionRouter|SourceReadToken.*|ExpiredWorkSourceSelectionSkipsOrphans|WorkSourceCommandReceiptsThroughRouter|SweepExpiredWorkSourceCommands|DaemonRegistration.*ThroughRouter)$" -count=3 -v'
```

Browser/visual testing remains user-owned. No new graph controls are available. Full graph orchestration, addressed mail/handoffs, product session controls, durable restart replay and preferred jj execution remain unfinished.

## Milestone 9: nonexecuting receipt-derived drafts

Published code `a725d71ed866ffb3cda2d8dcd21834548d117c8b` includes the draft feature `1a1e78ab23133080e3e093b943f7f81aecb3064a` and concurrent-index retry recovery. Owners/admins can create a frozen graph through `POST /api/workflow-runs`; workspace members can retrieve it through `GET /api/workflow-runs/{id}`. See [workflow-drafts.md](workflow-drafts.md) for the exact request, limits, error policy and cleanup semantics. There is no graph page, Start button or agent execution in this milestone.

The corrected immutable archive passed shared client tests/typecheck/lint, graph and source service checks, Beads/daemon/CLI/config/handler race checks, concurrent migration contract checks, public-router/PostgreSQL draft/receipt/registration suites three times, the actual TypeScript client bridge three times, and approved actual Beads/daemon qualification three times. Both CLI and server built, vet passed and CLI help ran. The first archive attempt failed because new concurrent indexes lacked retry cleanup registration; the correction was verified before publication. Earlier frontend packaging remains the separate milestone 7 result, not a new draft UI build.

On a clean published testing checkout, apply reviewed migrations 597 through 599 with that checkout's managed connection as described below. Do not apply unrelated pending migrations from an implementation tree. Then run:

```bash
devenv shell -- bash -c 'cd server && go test -race ./internal/service -run DraftGraph -count=1'
devenv shell -- bash -c 'cd server && go test ./cmd/migrate -run "^Test(EveryConcurrentUpBuildHasCleanup|EveryConcurrentDownBuildHasCleanup|ConcurrentIndexCleanupsMatchTheirMigrations)$" -count=1'
devenv shell -- bash -c 'cd server && go test -race ./cmd/server -run "^TestWorkflowDraft" -count=3 -v'
devenv shell -- bash -c 'cd server && MULTICA_RUN_DRAFT_CLIENT_CONTRACT=1 MULTICA_SOURCE_CLIENT_PNPM="$(command -v pnpm)" go test -race ./cmd/server -run "^TestWorkflowDraftFrontendContract$" -count=3 -v'
```

The broad draft command skips the explicitly gated actual-Beads/client tests unless their qualification environment is supplied. The explicit client command above requires three passed client cases per iteration, with no skips. Default public-router tests use disposable fixtures and test-created executables, not installed agents. Actual Beads qualification requires the separate opt-in setup in [qualified-dependency-reads.md](qualified-dependency-reads.md), never automatic discovery.

For a manual API check, use succeeded complete detail receipts from the same source/configuration for the root and every reachable predecessor, then submit the documented request as an owner/admin. Expect 201 and a frozen `draft`; repeat with the same request UUID and reordered receipt IDs for 200 on the same run UUID. Changing capacity with that UUID must return 409. A member can GET the frozen graph but cannot create it. Do not use a real user source for deletion tests. A saved draft does not start workers, create Issues or write Beads data.

Browser/visual checks remain user-owned: repeat milestone 7's Sources checks and existing workspace/Issue/chat navigation on the published revision. Do not look for absent graph controls. Full parallel admission, joins, mail/handoffs, product session controls, durable restart replay and preferred jj execution remain unfinished.

## Get the published milestone

Use a separate clean checkout for periodic testing so ongoing implementation files cannot leak into the test build:

```bash
jj git clone git@github.com:smdex/multica.git ../multica-milestone
cd ../multica-milestone
jj git fetch --remote origin --bookmark main
jj new main@origin
```

For later updates in that disposable checkout, fetch and create a new working-copy change from `main@origin` only after preserving any edits you made. Do not abandon your test changes or overwrite your implementation checkout.

## Start and verify locally

Follow `CONTRIBUTING.md` for prerequisites. This repository declares a devenv environment. Use only that manager in a checkout that already uses it:

```bash
devenv up -d
devenv processes list
```

Inspect `devenv.nix` and any checkout-local override for the web/API addresses. Use the web address emitted by the development server, not an assumed port. On a fresh testing checkout, apply the migrations from the published revision through the managed shell before using the API:

```bash
devenv shell -- bash -c 'cd server && go run ./cmd/migrate up'
```

The migration command must use that checkout's managed `DATABASE_URL`. If authentication fails, inspect the declared service and its connection settings, do not reset passwords or create an assumed database. Do not run blanket migrations from an active implementation checkout without first reviewing its pending changes. Command receipt migrations 589/590/594/595/596 were selectively applied and verified for milestone 2.

Run the nonvisual acceptance suite with that managed connection:

```bash
devenv shell -- bash scripts/check-native-swarm-foundation.sh
```

The script fails if required tests are missing or skipped. It does not start services, apply migrations, run browsers, discover installed agent CLIs, or contact provider accounts. It tests PostgreSQL receipts/source lifecycle and the production Session interface with controlled fixtures. A pass is not full swarm acceptance.

## User-owned browser checks

No browser E2E or visual tests are run by the implementation agent. For these foundations and receipt milestones, check existing application behavior rather than looking for an undelivered swarm page:

1. Sign in using your existing local development account and open a workspace.
2. Verify existing Issue lists, Issue detail and project navigation still render and preserve workspace context.
3. Verify existing chat history, interaction controls and reconnect behavior render correctly. Do not launch a real provider-backed run unless you intentionally authorize its cost.
4. Check narrow windows, long titles, overflow, keyboard navigation and visible error states on these existing screens.
5. Record the published `main` commit ID, affected route, reproduction steps and screenshots for any regression.

For milestone 7, also test the shared Sources workflow manually:

1. Configure an approved local binding and intentionally start its daemon as in milestone 6. Open Sources in Web and desktop. Confirm merely opening/selecting a source launches no read, then use Refresh explicitly and inspect a listed item with Read.
2. Verify an ordinary member can select a completed history receipt but cannot request a new source read. A disabled source must remain selectable for history, with new reads disabled. Sign in with existing accounts, do not reset passwords.
3. Interrupt the read request response and use Retry. Confirm the same command/request UUID is reused and results become visible. Pending/claimed/unknown commands must prevent a second read. Stop the daemon or make the source unavailable and check the visible failed receipt and generic diagnostic. Do not expose local binding paths or tokens in screenshots.
4. For a passed deadline or failed receipt fetch, use Recheck. It must show the server's status rather than assume failure. Change workspace/source and verify results cannot bleed across scope. Test source rename/configuration refresh without losing the server receipt history.
5. Check keyboard labels/focus, long native IDs/titles/revisions, narrow-window wrapping/scrolling and all supported languages (English, Simplified Chinese, Japanese, Korean and French). Desktop Sources must remain a normal workspace tab, not a pre-workspace overlay.
6. Record the published commit ID, route, account role, observed receipt UUID/status, reproduction steps and screenshots for any regression. Never record bearer credentials.

Graph controls are still undelivered. The future browser checklist will cover source-only execution without synthetic Issues, source rename/link/unlink UI, parallel A/B with gated C, addressed mail/processed receipts, same-session input, Hold/Cancel/restart, permissions and partial outages once those workflows exist. Do not count these future checks as passed or ask the user to test missing controls.

## Milestone 10: scoped native enrollment authority

[The enrollment authority milestone](native-enrollment-authority.md) records the verified backend routes, exact production-router/PostgreSQL outcomes and reproducible nonvisual checks. At that backend-only checkpoint, local CLI/daemon delivery remained under acceptance; milestone 11 below supplies its verified operational increment. Enrollment does not initialize Beads or start a graph.

## Milestone 11: operational dedicated enrollment

The [operational enrollment ledger](native-enrollment-operational.md) records the exact verified CLI/domain/daemon and shared Sources metadata commits, actual public acceptance checks and operator/visual instructions. The native domain remains empty and disabled after enrollment: its manifest hash identifies the ownership marker, not Beads content or graph readiness. Explicit CLI and production Daemon.Run enrollment, lock ownership, exact retries, cancellation, authority fences and the real TypeScript client bridge passed immutable archive checks. No browser E2E was run. Native initialization, graph Start/admission, typed handoffs/mail, Web session control/restart and preferred jj execution remain unfinished.

## Milestone 12: qualified dedicated source initialization

The [native source initialization runbook](native-source-initialization.md) supersedes milestone 11's statement that initialization is unfinished. The optional approved executable initializes a fresh dedicated domain while the original ownership lock is held, before profile-index publication. Verified replay, refused-launch, partial-init and authority paths are documented there. This is Linux-qualified local initialization, not source write CAS, external quiescence or graph admission.

## Milestone 13: native admission safety foundations

Published queue code `d26ac691` and launch/lifetime code `7126ed35`, followed by acceptance documentation `e8829132`, add [native graph admission safety foundations](native-graph-admission-foundations.md). The exact code passed real PostgreSQL queue and public-path compatibility checks, durable local journal and borrowed-domain race checks, and opted-in actual fake-Claude Session namespace qualification. Independent bounded review found no concrete blocker. Migrations 602-604 were reviewed and selectively applied for these checks.

This milestone does not activate graph Start or advertise graph execution capability. There is no new graph page or control to visually test. Follow the linked requirement-to-check mapping and operator commands for nonvisual checks, and retain the existing Sources/enrollment and workspace/Issue/chat visual checklist above. Graph scheduling, joins and typed handoffs/mail, same-session product controls, durable restart reconciliation and preferred jj runtime execution remain unfinished.

## Publication boundary

Verified milestones are published with jj to `origin` (`smdex/multica`) on `main`, with a dry-run and fast-forward ancestry check. Unfinished worker files and unrelated local edits stay local. Private agent transcripts are excluded from the published milestone ancestry. The original local history and working copy are preserved.
