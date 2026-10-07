# Qualified outgoing dependency observations

## Contract and limits

Qualified on October 7, 2026 against Beads 1.3.1, pinned upstream commit `c1c4b642ac1c08d8c828007a1c2f96e47e43ef7c`, using an explicitly allocated disposable embedded-Dolt source. No user source or installed agent CLI was accessed.

`show` cannot establish complete dependencies. Its dependency and count retrieval errors are swallowed upstream, including a misleading zero count on count failure. Matching counts, an empty array, or a source-provided completeness flag are not sufficient evidence.

The supported strict operation is:

```sh
bd --readonly --sandbox dep list --direction down --json -- "$native_id" "$native_id"
```

The repeated exact anchor selects the public raw-edge batch operation. It preserves dangling and external endpoints and all relation types. The hydrated single-anchor operation can omit external endpoints and must not be used for completeness. A nonexistent anchor can return successful exit and `[]` with warnings, so any stderr, including whitespace, is failure.

The client accepts only one non-null bounded JSON array, at most 512 edges, each with the exact source ID, a nonblank opaque target and relation type, and no duplicate target/type tuple. The metadata and edge operations share one 30-second deadline, preserving an earlier caller deadline. Raw operation diagnostics are not exposed in returned errors. The daemon reports a fixed generic failure diagnostic.

A successful detail receipt retains `dependencies` (opaque predecessor ID and exact `dependency_type`) and `dependencies_complete:true`. Empty complete observations explicitly contain `dependencies:[]`. Server report validation requires an explicit matching dependency count and validates the bounded typed evidence again. Older receipts without completeness remain readable and byte-compatible but cannot establish graph closure.

**Completeness applies only to that outgoing-edge read.** Metadata and edges are separate observations, not an atomic snapshot. Cross-item reads are not atomic. In the disposable fixture, adding dependencies did not change the item's revision. Item revision is not topology CAS. Unknown relation kinds and external endpoints remain data, not successful execution producers. An eventual Multica graph digest identifies an observation set, not a source revision.

## Requirement-to-check map

| Requirement | Runnable check | Observed result before immutable acceptance |
| --- | --- | --- |
| Strict raw argv, exact source and opaque typed endpoints | `TestReadTaskRawArgvAndEnv`, `TestReadTaskDependencyEvidence` | Fake-only unit checks passed. |
| Reject missing anchor warnings, null/trailing/malformed/duplicate/foreign/oversized output | `TestReadTaskDependencyEvidence`, `TestReadTaskDependencyBound`, `TestReadTaskRawCancellationAndOutputCap` | Fail-closed matrix passed, including diagnostic leak checks. |
| Shared whole-operation timeout | `TestReadTaskSharesWholeOperationDeadline` | Reproduced failure with separate invocation budgets, then passed after shared context fix. |
| Legacy canonical bytes and complete empty array | `TestWorkSourceCommandLegacyCanonicalBytes`, `TestWorkSourceCommandDependencyReport` | Pure validation checks passed. |
| Report consistency and topology distinction | `TestWorkSourceCommandDependencyBound`, `TestWorkSourceCommandTopologyChangesCanonicalResult` | Pure validation checks passed. |
| Actual supported CLI, join and empty leaf | `TestReadTaskThroughQualifiedCLI` | Actual approved Beads executable passed under race detector against disposable source, not a copied parser or CLI stub. |
| Typed detail delivery and retry without reexecution | `TestSourceReadDaemonDispatchThroughProductionRouter` | Actual daemon and production HTTP/PostgreSQL passed all five variants: list, lost list reply, detail, lost detail reply and source failure. Test-created upstream and agent executables only. Detail launched show and raw-edge subprocesses once each. |
| Actual upstream through public delivery boundaries | `TestSourceReadQualifiedDaemonThroughProductionRouter` | Actual daemon, approved Beads executable, production router and PostgreSQL passed with three exact outgoing edges and a lost committed terminal reply. Only two upstream launches, no reexecution. |
| Public legacy replay and changed topology conflict | `TestSourceReadDependencyReceiptsThroughRouter` | Passed through production HTTP/PostgreSQL under `-race -count=3`: legacy additive fields remain omitted, identical terminal report/create retries return 200, changed topology with unchanged item revision returns 409, and rejected reports leave accepted bytes unchanged. |

## Explicit opt-in actual CLI checks

Default tests use test-created executables and never resolve installed agent CLIs. The actual Beads checks require operator-authorized disposable input and these explicit environment selectors:

```sh
export MULTICA_RUN_BEADS_QUALIFICATION=1
export MULTICA_BEADS_EXECUTABLE=/absolute/approved/bd
export MULTICA_BEADS_DIRECTORY=/absolute/disposable/.beads
export MULTICA_BEADS_ROOT=fixture-root
export MULTICA_BEADS_A=fixture-a
export MULTICA_BEADS_B=fixture-b
export MULTICA_BEADS_EXTERNAL=external:qualification:no-producer
(cd server && go test -race ./pkg/beads -run '^TestReadTaskThroughQualifiedCLI$' -count=1 -v)
(cd server && go test -race ./cmd/server -run '^TestSourceReadQualifiedDaemonThroughProductionRouter$' -count=1 -v)
```

The router test additionally needs the checkout's managed database. Do not create a second database/service manager. The fixture must already contain root-to-A, root-to-B and root-to-external `blocks` edges. These tests only read that fixture. The actual daemon's agent provider remains a test-created inert executable, not an installed/account-backed agent.

This slice does not implement graph admission, Start, source writes, source execution ownership, mail, handoffs or durable restart replay. Browser and visual acceptance remain user-owned.
