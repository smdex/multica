# Operational native source enrollment

Date: October 7, 2026. This slice adds dedicated local ownership and daemon enrollment to the [scoped authority milestone](native-enrollment-authority.md). It is not full native swarm delivery. Enrollment does not initialize Beads, attest source contents, prove process-tree quiescence, or permit graph Start.

Exact code commits `1207e18944361d925b63bd5fbcc07730f4110141` (CLI/domain/daemon) and `79f23bb3e8c16920e5baca6a547f54a261d5c647` (shared metadata/UI/client bridge) passed immutable archive acceptance on October 7 in task `6042156gfk`. The archive-built CLI, production Daemon.Run, enrollment authority/lock waits and actual TypeScript HTTP client each ran their explicit public integration gates, including three enrollment/client iterations without skips. Core/views types, changed-file lint, race suites, compatibility, migration cleanup, vet and both builds passed. Browser/visual E2E and full graph execution were not run.

## Manual operator check

Use a disposable testing workspace and the published CLI/server build. Authenticate the selected CLI profile as a current workspace owner/admin who also owns the exact online runtime. Use the same backend, profile and daemon identity throughout. Starting or restarting a daemon can probe configured agent executables and execute ordinary queued tasks. Use only runtimes and queued work you intentionally approve.

1. Record the workspace and owned runtime UUIDs. Choose one fresh request UUID and keep it for retries of this exact name/runtime request.
2. Provision a new source:

   ```bash
   multica --profile dev daemon source provision \
     --workspace '<workspace UUID>' --runtime '<owned runtime UUID>' \
     --name 'native enrollment check' --request-id '<stable request UUID>'
   ```

   Expect a JSON source receipt with `mode: "native"`, `enabled: false`, `native_enrollment_status: "pending"`, a source UUID and an enrollment UUID. The command creates a fresh dedicated directory under `~/.multica/native-sources/<source UUID>`, not an existing project directory. Selected profiles have separate indexes and approval inboxes, but physical domains are machine-global. Do not move, rewrite, loosen permissions on or delete ownership markers to bypass a refusal.
3. Restart the same daemon profile after provisioning, or start it with `multica --profile dev daemon start --foreground`. The daemon must register and track that exact workspace/runtime pairing and acquire the domain's OS ownership lock.
4. While it is running, approve the returned source UUID:

   ```bash
   multica --profile dev daemon source approve \
     --workspace '<workspace UUID>' --source '<source UUID>'
   multica --profile dev daemon source status \
     --workspace '<workspace UUID>' --source '<source UUID>'
   ```

   Approval prints only the source UUID and expiry, never a credential. It places a short-lived capability in the selected profile's private ephemeral inbox. The running daemon checks the held ownership marker and submits the scoped finalize request. Expect the status receipt to become `enrolled` with the same source/enrollment identities and a manifest hash. This hash identifies the ownership marker, not Beads data. Approval expires within 120 seconds. If it expires before finalization, inspect daemon availability and rerun `approve`; do not change identities or force-delete local state.
5. Repeat `provision` with the exact same request UUID/body while idle and while the daemon holds the lock. Expect the same source/enrollment UUIDs without replacing local state. A changed body with that UUID is rejected. If server intent succeeded but local provisioning failed, retry the identical request UUID after addressing the reported local problem. Torn or conflicting domains/indexes deliberately require operator investigation rather than destructive automatic repair.
6. Stop the testing daemon normally. The ownership lock must be released before `Daemon.Run` returns, while the marker and profile index survive. These commands refuse inside daemon-managed task context before HTTP or global filesystem access.

If your daemon was started with an explicit daemon identity override, supply the identical `--daemon-id` to these commands. Never paste capability tokens or private approval files into logs, screenshots or issue reports.

## Visual checks delegated to the user

Open `/{workspaceSlug}/sources` on web and desktop. A pending native source should display Pending enrollment, an enrolled source should display Enrolled, and unknown newer-backend values must not look enrolled. Switching sources/workspaces must not leak the prior selection's identity or receipt. The page is read-only for native metadata and has no native graph Start or enrollment button. Check narrow layouts, long source names, dark/light themes and the supported translations. No browser or visual E2E was run by the coding agent.

## Requirement-to-check mapping

| Changed behavior | Runnable check | Observed acceptance boundary |
| --- | --- | --- |
| Fresh private domain, immutable marker/index, strict replay, no alias/adoption | `go test -race ./internal/daemon/execenv -run NativeSource` and `go test -race ./internal/daemon -run NativeSource` | Real OS roots/files/locks with temporary HOME, strict identity/hash agreement, permissions and create-only publication. |
| Human CLI and actual daemon enrollment | `TestNativeSourceEnrollmentDaemonRunAcceptance`, with `MULTICA_NATIVE_ENROLLMENT_TEST_CLI` pointing to the built checkout CLI | Actual CLI subprocess, production HTTP router/PostgreSQL and `Daemon.Run`; pending to enrolled, approval while OS lock is held, exact replay and bounded cancellation. Test-created agent executable only, no provider account. |
| Managed tasks cannot provision | `TestNativeSourceEnrollmentCLIRefusesManagedTask` | Actual CLI refuses before HTTP and leaves HOME/.multica absent. |
| Marker failure retains authority rather than dropping the lock | `TestNativeSourceEnrollmentLoopMarkerMismatchRetainsLock` | Explicit reconciliation passes reach the marker-failure branch and a second OS open remains exactly Busy. Actual cancellation is covered separately. |
| Current owner, scope, expiry and membership incarnation | Six authority tests documented in the linked milestone | Production router/PostgreSQL lock waits and real membership removal, not an injected handler. |
| Native fields survive the public shared API boundary | `TestSourceReadFrontendContract`, with `MULTICA_RUN_SOURCE_CLIENT_CONTRACT=1` and an absolute `MULTICA_SOURCE_CLIENT_PNPM` | Production intent/mint/finalize prepare pending/enrolled rows, then the checked-out TypeScript ApiClient validates actual list responses. All seven TS tests must pass without skips. |
| Shared web/desktop caption and conservative unknown state | `packages/views/work-sources/components/sources-page.test.tsx` and `packages/core/api/work-source-client.test.ts` | Component wiring and schema regressions. Visual behavior remains user-owned. |

## Reproduce nonvisual acceptance

Run through the checkout's existing managed environment and authorized test database. Do not start another database manager or apply unrelated pending migrations. Only migrations 600 and 601 belong to enrollment authority; this operational slice adds no migration.

```bash
devenv shell -- bash -c '
  set -e
  cd server
  export MULTICA_NATIVE_ENROLLMENT_TEST_CLI="$PWD/multica-enrollment-check"
  go build -o "$MULTICA_NATIVE_ENROLLMENT_TEST_CLI" ./cmd/multica
  go test -race ./cmd/server -run "^TestNativeSourceEnrollment" -count=3 -v
  MULTICA_RUN_SOURCE_CLIENT_CONTRACT=1 MULTICA_SOURCE_CLIENT_PNPM="$(command -v pnpm)" \
    go test -race ./cmd/server -run "^TestSourceReadFrontendContract$" -count=3 -v
'
```

The explicit CLI/client test gates fail if the managed database is unavailable. Ungated ordinary suites may skip these opt-in process/client tests, so their exit code alone is not operational acceptance. Full graph execution, typed handoffs/mail, Web session control/replay, uncertain-process restart recovery and selected jj execution remain unfinished requirements.
