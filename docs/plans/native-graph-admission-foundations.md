# Native graph admission safety foundations

Status: safety foundations only. Graph Start, node scheduling, typed handoffs, restart reconciliation and full swarm execution are not activated by this milestone. Existing source reads, enrollment and frozen draft APIs remain the operational features. Browser and visual acceptance remain operator-owned.

## Queue isolation

Migrations 602-604 add nullable graph/source/native identities and an `execution_uncertain` flag to the existing task queue. There are no new foreign keys.

- Legacy rows have no graph identity and cannot assert execution uncertainty.
- Graph rows require complete nonzero graph/source identities, a nonblank native item, an agent and a runtime. They cannot masquerade as issue, chat, autopilot or wakeup tasks.
- The source/native active-slot index prevents two roots from simultaneously reserving the same item. A failed attempt marked execution-uncertain still occupies that slot.
- The run/native node-slot index also covers queued attempts and prevents duplicate live attempts within a run.
- Both indexes are concurrent single-statement migrations registered with the migration runner's recovery map.

The existing SQL query families exclude graph rows from legacy claim, start, retry, cancellation, settlement, runtime reassignment, orphan recovery, timeout recovery and deferred promotion. Candidate listing and deferred polling also exclude them. `CountRunningTasks` intentionally includes uncertain graph attempts even after a terminal status, so legacy work cannot reuse their capacity.

The static guard inventory contains 48 queries in `agent.sql`, four in `runtime.sql` and one in `supplement.sql`. This count includes read and capacity fences, not 53 distinct mutation workflows. Six insert-only legacy writers omit graph identities. Existing chat/wakeup/autopilot and workspace deletion paths were classified separately by their identity predicates. This static classification is not proof that future graph activation safely handles teardown. Before activation, source/workspace deletion and runtime revocation must preserve or reconcile uncertain live attempts rather than erase evidence.

## Durable local launch evidence

A journal under the already-held native domain root stores an exact task/incarnation binding. No pathname reopening or second ownership lock is used.

1. `reserved.json` is durably published before the server Start transition.
2. `launching.json` is durably published before attempting provider execution. Only the create-only winner may attempt launch.
3. `stopped.json` records trusted positive evidence: a qualified namespace-init Wait or a synchronous failure proving the provider executable did not execute.

The parent directory is synced after directory creation. Every successful publication and identical replay syncs the containing directory before acknowledgement. Identity, private permissions, file type, size, JSON shape, timestamp metadata and filename/state agreement are validated. Stop requires the reserved and launching chain. Corrupt, foreign or missing evidence fails closed. There is no timeout-driven reset, PID reuse inference or assumption that a missing launched record means never launched.

The journal has an 8 KiB record bound and bounded opaque selectors. A future admission caller must validate those bounds before creating a queue reservation. The ownership marker hash authenticates local marker bytes only, not source content or physical identity. Journal Stop is an internal trusted-attestation seam, not an API accepting worker-supplied stop claims.

## Held-domain lifetime

The enrollment loop exposes an internal borrow of its exact held domain and local marker hash. Shutdown first fences new borrowers, then waits for current borrowers before releasing the ownership lock. Borrow release is idempotent. A future graph execution caller must keep the borrow until it has persisted qualified stop evidence or durable uncertainty. The helper is not yet connected to a graph executor.

## Qualified process seam

Only the Claude backend implements the opt-in `NativeGraphProcess`/`BeforeLaunch` seam. It configures Linux user and PID namespaces with identity-preserving UID/GID mappings and the existing process-group setup. The durable launch callback must complete before `cmd.Start`.

Positive `Result.NativeProcessStopped` is set only after actual namespace-init `cmd.Wait` returns with a process state. A semantic result, stdout EOF, heartbeat loss or process-group ESRCH is not substituted for it. False means no proof, not proof that a process is still running. A synchronous launch failure proves the provider executable never executed, not that no internal fork occurred.

This is process-stop evidence for processes inside the namespace. It is not filesystem or network isolation and does not cover work delegated to external services. Other providers do not honor this option and must not be admitted by a future graph capability gate. Non-Linux opted-in execution refuses before provider execution. Deployed Linux environments must qualify actual namespace permission and behavior, not merely inspect `/proc` or a kernel version.

## Requirement-to-check mapping

| Requirement | Runnable check and observed acceptance boundary |
| --- | --- |
| No partial/legacy-looking graph identity | `TestGraphQueueIdentityCannotMasqueradeAsLegacyKinds`, actual PostgreSQL CHECK failures |
| Cross-root exclusivity survives uncertain terminal state | `TestGraphQueueSlotsPreserveUncertainExecution`, actual unique-index violations and release after certainty clears |
| One live attempt per run/node including queued state | `TestGraphQueueNodeSlotIncludesQueuedAndUncertainAttempts`, actual index violations |
| Legacy workflows preserve graph rows without breaking legacy rows | `TestGraphTaskLegacyFenceMatrix`, whole-row JSON equality plus matched legacy controls through generated SQL |
| Independent start, failure, rebind and sweep positive controls | Six `TestGraphFence*Controls` tests with actual returned legacy state and exact matching inputs |
| Graph deferred rows do not drive legacy polling | `TestGraphDeferredPollingIgnoresGraphRows`, exact legacy deadline then no deadline after deleting the legacy twin |
| Uncertain graph execution still reserves legacy capacity | `TestCountRunningTasksIncludesUncertainGraphReservation`, count 1 then 0 only after certainty clears |
| Exactly one durable launch winner; no stop inference from gaps | `TestNativeGraphReservation*`, real private files and concurrent create-only launch transitions |
| Owner lock survives enrollment-loop shutdown while borrowed | `TestNativeSourceBorrow*`, actual enrollment loop and competing domain opens before/after release |
| Same actual Session gives positive scoped stop evidence | `TestNativeGraphProcessNormalCompletionRunsInNamespace` and `TestNativeGraphProcessCancellationStopsEscapedGrandchild`, retained pidfd alive-before/dead-after for a setsid descendant |
| Bad evidence and refused launch do not prove execution | `TestNativeGraphProcessInvalidPidfdIsNotStopEvidence`, missing/callback failure markers and positive marker control, LookPath vs Start callback counts |
| Legacy provider behavior remains unchanged | Existing `TestClaude*` tests and zero-value option regression |

The queue suite uses real PostgreSQL and real generated query interfaces. It does not constitute public graph API acceptance because no such execution API is activated. Process tests use the actual Claude Session implementation with a test-created fake executable, never an installed or authenticated agent CLI. Namespace tests are explicitly gated and fail rather than skip when opted in but unsupported. The SQL matrix covers representative workflow paths, not an individual dynamic test for every static guard.

## Operator checks

Use the checkout's existing managed environment and database configuration. Do not start a second database manager or apply all pending migrations indiscriminately.

```sh
make sqlc
cd server
go test -race ./internal/service -run '^(TestGraphTaskLegacyFenceMatrix|TestGraphFence.*Controls|TestGraphDeferredPollingIgnoresGraphRows|TestCountRunningTasksIncludesUncertainGraphReservation|TestGraphQueue.*)$' -count=3
go test -race ./internal/daemon -run '^(TestNativeGraphReservation|TestNativeSourceBorrow|TestNativeSourceEnrollmentLoop)' -count=3
MULTICA_NATIVE_GRAPH_PROCESS_TEST=1 go test -race ./pkg/agent -run '^TestNativeGraphProcess' -count=3
go test -race ./pkg/agent -run '^TestClaude' -count=1
go vet ./internal/service ./internal/daemon ./pkg/agent
go build ./...
```

The PostgreSQL tests require the managed test database to be reachable. Inspect output for skipped integration tests, not just exit status. There is no new visual feature or Start control to test in this increment. Use the existing Sources/enrollment runbooks for operational UI checks. Never downgrade away graph identity or indexes while attempts or uncertain execution exist.
