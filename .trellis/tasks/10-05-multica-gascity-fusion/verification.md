# Native swarm verification

Status: implementation in progress. Workspace maps to City, Project maps to Rig. The existing TaskService and daemon remain execution owners. Passing the foundation checks below does not establish completion of T01-T16 or user-visible swarm acceptance.

## Managed environment

Use the process manager already selected by this checkout. This checkout has declared `devenv` services, so inspect them with `devenv processes list` and run commands through `devenv shell --`. Do not run `make up`, reset passwords, or create an assumed database to fix a test connection.

On October 7, the shell's default DATABASE_URL failed to authenticate against the database it targeted and the handler package exited successfully after skipping all tests with SQLSTATE 28P01. The declared service was verified through its Unix socket using its existing local role. Neither credentials nor services were changed. The foundation script explicitly fails on database/test skips.

For this checkout, the following observed connection worked. Recheck the socket in `.devenv/state/postgres/postmaster.pid` if the managed environment changes. No credentials are included:

```bash
devenv shell -- bash -c '
  export DATABASE_URL="postgresql://smaximov@/multica_multica_533?host=/run/user/1000/devenv-3a42bb8/postgres"
  cd server && go run ./cmd/migrate up
'
devenv shell -- bash -c '
  export DATABASE_URL="postgresql://smaximov@/multica_multica_533?host=/run/user/1000/devenv-3a42bb8/postgres"
  bash scripts/check-native-swarm-foundation.sh
'
```

Migration commands apply pending migrations to the declared development database. Inspect the pending migrations first. The check script does not apply migrations, start services, discover installed agent CLIs, or call provider accounts. Tests use repository fixtures and clean up their own records.

## Observed foundation checks

| Requirement | Real check | Observation |
| --- | --- | --- |
| Committed batch replay has one receipt and one transcript copy | `TestReportTaskMessagesIdentifiedBatchReplay` against actual PostgreSQL | Passed. Same UUID/payload replays, changed payload returns 409. Expanded test also asserts one fanout event. |
| Lost HTTP receipt after a real commit is safely retried | `TestReportTaskMessagesLostReceiptThroughRealClient` with actual client, token middleware, HTTP handler and PostgreSQL | Passed under `-race`: hidden first receipt, exactly two POSTs, one persisted message and stable batch ID. |
| Concurrent retries cannot create duplicate rows | `TestReportTaskMessagesIdentifiedBatchConcurrentReplay` | Passed with four concurrent handler requests. |
| Batch identity is scoped to the task | `TestReportTaskMessagesIdentifiedBatchTaskScope` | Passed. Same UUID accepted independently for two tasks. |
| Equivalent decoded JSON has stable identity | `TestTaskMessageBatchIdentityUsesDecodedFields` | Passed in root run 210201ghur. Whitespace/key ordering preserve the hash, changed logical content changes it. |
| Capability discovery preserves authorization | `TestTaskMessageCapabilitiesAuthorization` | Passed after correcting the test to existing cross-workspace resource-hiding 404, with unauthenticated 401. |
| Invalid identified batches fail before persistence | `TestReportTaskMessagesIdentifiedBatchValidation` | Invalid UUID, empty batch, duplicate, negative and overflowing sequence rejected. |
| Legacy batches, event ordering, clocks and NUL sanitation remain compatible | Existing `TestReportTaskMessages*` tests | Passed against actual PostgreSQL. |
| A failing insert cannot partially persist messages | `TestCreateTaskMessagesBatchIsAtomic` | Passed using the real production SQL query. |
| Existing Session accepts input and captures resulting output without starting again | `TestWorkflowRunSteersTheBoundSessionWithReplaySafety` through production backend/Session callbacks | Passed under `-race`: definite and lost acknowledgements retain one PID/start nonce, replay sends once, output reaches the original Session, and cancellation reaps the test process. |
| Source lifecycle and workspace/admin gates | `TestWorkSourceLifecycle`, `TestWorkSourceHandlerGuards` | Passed against PostgreSQL. Conflicts are rejected, renaming keeps a disabled source disabled, and foreign-workspace access is hidden. Root expanded script 592863kf72 also passed the opaque-ID preservation assertions. |
| Source owner cannot disappear during teardown | `TestWorkSourceRuntimeTeardownRefusal`, `TestWorkSourceRuntimeDeleteEndpoint409`, `TestRuntimeGC_KeepsWorkSourceOwner` | Root run 210201ghur passed: explicit delete returns 409 and GC retains the owning runtime. |
| Parent deletion preserves correct authority boundaries | `TestWorkSourceParentDeletion` | Root run 290727ipsm passed: project deletion detaches the binding and preserves links with revision increment, issue deletion sweeps its links but retains the source, workspace deletion sweeps both metadata tables and retains another workspace's source. This does not execute or prove deletion safety inside an actual Beads store. |
| Legacy runtime merge cannot orphan a source | `TestWorkSourceRuntimeMergeRefusal` plus existing merge fence/happy-path/rollback regressions | Root run 290727ipsm passed. Bound owner is retained, source-free merges still work, failing steps roll back. |

The receipt/database and same-process fixture passed under `-race`. The earlier receipt-only script passed with the declared service connection and rejected a deliberately unreachable database's otherwise-successful skipped test run. Root serial runs 210201ghur and 290727ipsm passed the named source, parent teardown, GC, receipt and legacy regression checks without skips. The exact expanded user script passed on October 7 (592863kf72, exit 0), requiring each listed acceptance test and rejecting skips. Intermediate compilation failures and cancelled overlapping runs are not acceptance passes. Index cleanup registration passed separately, including migrations 584-588.

The broader producer suite exposed a real empty-session capability negotiation regression and a slow oversized-batch redaction path (945991t26l, cancelled after failure/stall). Capability negotiation now runs only before a nonempty send, capture applies a conservative raw-size bound before local redaction, and capture-failure one-shot sends retain the original payload without redundant local redaction. Root run 1761184amy passed the complete selected `TaskMessage|ExecuteAndDrain|Supplement` suite under `-race` and daemon vet. Its large capture-failure case uses a fake HTTP transport, not server-side giant-redaction acceptance. The hardened source delete/link race captures both operation errors, requires successful deletion and asserts both source and link absence. It passed in expanded script 592863kf72, and its revised snapshot DN2nfyTo was independently read and accepted. Independent receipt and runtime guard reviews reported no blocking code defects, with source-required coverage consumed. This is handler/database and Session-interface integration, not deployed browser/daemon swarm acceptance. Receipt state shares transcript retention. Incarnation authorization and restart replay remain separate T08 work, not implied by a batch UUID.

The read-only Beads adapter is committed in 358c4a4cd. Its 23 fake-executable tests passed under `-race` with vet, including a JSON-null regression observed failing before correction. Earlier disposable Beads 1.3.1 qualification exercised actual list/show and the real Go client. Revision CAS and create attribution were not qualified, so source writes remain disabled. The embedded Dolt mode does not qualify standalone Dolt history operations.

Producer capture is committed in 1bd2b2795. Scoped source identity, teardown fences and core API compatibility are committed in e143d39fe. Root core run 2444728k9x passed 13 client tests, core typecheck and changed-file lint. Two missing/null capability-default cases failed before safe `false`/`0` normalization was added. These fetch-stub checks establish the client boundary, not a deployed browser workflow.

Source-command reads are an uncommitted worker checkpoint, not activated routes or accepted source browsing. Root read-only checkpoint review WUHs4HXW identified required runtime/daemon identity, enabled/configuration fencing, exact terminal replay, typed result validation, teardown integration and bounded read-claim recovery checks. Swarm status and both friendly-name and exact-session messages timed out repeatedly on October 7, 03:01-03:10 UTC. Delivery of that review is unconfirmed. No worker-owned source-command implementation was changed, shared agent service restarted, or debug control enabled. Do not overlap its owner or count this checkpoint as completed.

## Verified link-cardinality correction

The design permits many-to-many Issue/source-item relationships. The original migration 587 instead allowed each source/native ID pair to link to only one Issue. Additive migrations 592-593 build a unique relationship index over `(source_id, native_id, issue_id)` before removing that overly restrictive index. Exact duplicate relationships remain conflicts. Rollback deliberately refuses if multiple Issue links now exist for one source/native ID pair, rather than deleting user relationships.

`TestWorkSourceManyToManyLinks` exercises two Issues sharing one native item, an Issue linking multiple items, duplicate conflict, scoped lists and unlink preservation through the public handlers. Root run 667914tnpl initially compiled the handler suite, built the backend and passed concurrent-index cleanup registration checks. On October 7 at 12:36 UTC, run 596128vsaz reproduced the public-handler failure against the actual managed PostgreSQL database: linking a second Issue to the same native item returned 409 instead of 201. Run 627374wpb5 applied only reviewed migrations 592-593 through the production migration runner, using a scratch migrations directory and the existing managed database. Unfinished source-command migrations 589-590 were not applied. Run 645479qccq then passed the regression and seven related source lifecycle/teardown/authorization checks under `-race`, plus both concurrent-index cleanup registration tests, without skips. The foundation script now explicitly requires this regression and limits its source selection to accepted foundation tests rather than inactive command checkpoints.

The complete updated foundation script subsequently passed on October 7 at 12:40 UTC (7755638gjb), including the required many-to-many regression and skip detection. Browser/visual verification was not run. Source-command safety review resumed with a new read-only worker after coordination became available.

## Milestone publication checks

The sanitized milestone tree excludes `.pi-subagents` transcripts and inactive source-command files while preserving the original local history and working copy. It is based on the existing upstream-synced local main; the fork's old main is an ancestor, so publication is a fast-forward, not a forced rewrite. Run 999983j6z0 passed the exact archived milestone's complete foundation script and Beads adapter tests under `-race`. Its combined command then failed because the coordinator incorrectly requested nonexistent `./cmd/daemon`. This was a verification command error, not a product build failure. Corrected run 153374c5bo built the actual `./cmd/server`, daemon-owning `./cmd/multica` and `./cmd/migrate`, passed Beads/service vet and safely exercised CLI and daemon help without invoking installed agent CLIs or accounts.

Root-wide frontend typecheck initially failed (0401053vs3) because the private Next application requested unused declaration output and Fumadocs inferred Zod types could not be named portably. The web app now disables declaration/declarationMap, matching the docs app while retaining strict source checks. Run 3136098ow9 passed all 10 non-mobile typecheck tasks, 223 core API/chat tests and 327 views chat tests. Existing React `act` warnings were emitted, but no tests failed. Browser/visual checks and full frontend lint/tests were not run in these commands. User setup and visual-check instructions are in `docs/plans/native-swarm-milestones.md`. These checks qualify a foundations milestone, not completed orchestration or source-command delivery.

## Verified read-command receipt milestone

On October 7, migrations 589/590/594/595/596 were structurally reviewed and applied alone through the production runner with a scratch directory containing only those files (693283hc61). Concurrent up/down cleanup registration tests passed and the ledger contains exactly the five selected command versions. No unrelated pending migration was applied.

Initial real database run 7338543t5v caught a sibling-runtime fixture violating the actual workspace/daemon/provider unique key. The sibling uses a distinct test provider now, without relaxing product constraints. Stable run 0592862cn8 passed the command public-handler/service suite under `-race`, including deterministic runtime-before-source lock probes and the real public-schema deletion manifest. Its later HTTP router subcommand failed 404 before route activation, reproducing the integration gap rather than calling compile success acceptance.

| Changed contract | Runnable check and observed result |
| --- | --- |
| Stable request identity and bounded read-only input | `TestWorkSourceCommandCreateAllowlistAndGuards`: unsupported operations/invalid fields rejected, member cannot create, duplicate UUID returns same receipt, another in-flight request conflicts. |
| Exact daemon/runtime claim and report identity | `TestWorkSourceCommandOwnerRoutedClaimReport`: empty/foreign daemon context and sibling runtime denied, repeated claim conflicts, claimer-scoped identical terminal replay succeeds, opposite/changed reports conflict, terminal create retry keeps identity. |
| Enabled/configuration/runtime/deadline fences | `TestWorkSourceCommandRevisionDisabledAndExpired`: disabled, revised, offline and expired claims/reports refused, stale reports cannot revive expiry. |
| Typed bounded results | `TestValidateWorkSourceCommandReport` and `TestWorkSourceCommandCanonicalResult`: list/detail shapes, native/revision identity, duplicate IDs, limits, null/trailing JSON and oversized payloads checked; canonical equivalent results match. |
| No reverse-order workspace deadlock | `TestWorkSourceCommandWorkspaceRuntimeLockOrder`: PostgreSQL proves worker waits on runtime, workspace-style transaction obtains source/command `NOWAIT` for both claim/report, then releases worker successfully. |
| Source/workspace receipt cleanup | `TestWorkSourceCommandSourceDeletion` covers pending/claimed/succeeded/failed receipts, `TestWorkSourceCommandWorkspaceDeletion` preserves another workspace's receipt, `TestWorkspaceDeletionManifestCoversPublicSchema` covers new table. |
| Bounded terminal expiry | `TestSweepExpiredWorkSourceCommands` invokes actual server sweep twice: expired pending/claimed fail with no claimer/result, fresh pending and terminal success remain unchanged. |
| Actual route/auth/result transport | `TestWorkSourceCommandReceiptsThroughRouter` uses production `NewRouter`, real HTTP requests, JWT and DB-backed `mdt_` credentials. Unauthorized is 401, JWT/foreign daemon cannot claim, owner completes/replays, malformed result/opposite replay refused, missing/nonmember workspace concealed as 404, history omits result, HTTP source delete removes receipt. |
| Existing public compatibility | `TestIssuesCRUDThroughRouter`, `TestCommentsThroughRouter`, `TestWorkspacesThroughRouter` passed against the current production router and PostgreSQL. |

Run 2646221tmj passed the complete expanded `scripts/check-native-swarm-foundation.sh` without skips, all three legacy HTTP checks above, Beads adapter tests under `-race`, current server/multica/migrate builds and service/handler/server/Beads vet. Earlier router runs 144185s3um and 2106614qsa failed only incorrect test expectations that denied/missing workspaces return 403. The existing membership helper deliberately conceals them as 404, now asserted separately for real foreign and nonexistent workspace cases. Independent review 4Fnw7Mro accepted the repaired security/state checkpoint after consuming required source coverage.

This is a verified receipt API, not automatic Beads execution. The daemon has no approved handle mapping, dispatch loop or standalone `mdt_` bootstrap yet. Production's existing daemon PAT cannot prove machine identity, so new claim/report endpoints refuse it instead of weakening authorization. New unclaimed reads expire after five minutes. Graph admission, mail/handoffs, deployed same-session recovery and application jj lifecycle remain pending. Browser tests were not run and remain user-owned.

## Full product acceptance still required

On October 7 at 12:36 UTC the user assigned all browser E2E and visual testing to themselves. Agent verification is limited to code, CLI, unit and integration checks. Provide reproducible environment/setup instructions and a browser acceptance checklist; do not run Playwright or browser-agent testing. User ownership of the visual check does not imply that missing product workflows have been implemented.

T11 will provide the runnable application suite and fixture setup. Use TestApiClient for setup/teardown, current built server/daemon, a disposable Beads source and test-created workers through the actual Session interface. Do not replace the native coordinator with a fake daemon and call that full acceptance.

| User flow | Required observable evidence | Status |
| --- | --- | --- |
| Browse City/workspace and Rig/project work | Source-only Beads task visible without synthetic Issue, explicit link preserves Issue UUID, source rename preserves identity | Pending T02/T03/T10/T12 |
| Parallel swarm and join | A and B start once within capacity, C cannot start until valid success/handoffs, state survives restart | Pending T04/T05/T07/T11 |
| Agent messaging | Durable addressed mail, delivered distinct from processed, recipient replacement fences stale ack | Pending T06/T11 |
| Web-first session controls | Output tail and reconnect catch-up, input reaches same worker PID/start nonce, no second session | Same-process interface qualification passed, product T08/T09/T10/T11 pending |
| Cancellation/recovery | No starts after Hold/Cancel, unknown liveness stays reserved, stale attempt result rejected | Pending T05/T11 |
| Source outage and permissions | Other workspace denied, partial outage visible, unqualified source writes remain disabled | Pending T02/T03/T12 |
| Preferred jj | Real disposable jj workspace lifecycle, independent changes and recovery without modifying user repositories | Pending T13 |
| Compatibility and packaging | Existing Issue/chat/mobile API behavior retained, shared Web/desktop routes and malformed event tests pass on current build | Pending T10/review |

The existing `e2e/agent-workflow.playwright.config.ts` uses a fake workflow daemon. It is useful for existing UI wiring regressions but cannot prove native swarm scheduling or actual same-process session behavior. It is not the final swarm suite.

## Usage budget guardrails

User limits: check `usage-limits` regularly, stop all GPT use below 25% Codex remaining, and stop all work below 17% Codex remaining. Non-GPT alternatives are permitted above the global stop threshold. At October 7, 02:51 UTC, the command reported Codex weekly 34% remaining and GLM 61% remaining. Kimi quota was unavailable, not assumed usable. Preserve checkpoints and artifacts at a stop.

## Remaining source qualification limits

The initial adapter qualifies Beads 1.3.1 embedded-Dolt list/show reads in a disposable source. It exposes no writes because the inspected CLI lacks a revision CAS/create-attribution contract sufficient for safe synchronization. Standalone Dolt is not installed. Do not interpret embedded read qualification as successful Dolt history, write recovery, bidirectional sync, or genuine jj execution acceptance.
