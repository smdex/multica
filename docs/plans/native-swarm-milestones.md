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

These tests use test-created credentials and inert runtime providers, not installed agents. This milestone adds no migration or UI. Source-only credential bootstrap and automatic source-read delivery remain unimplemented.

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

Source/link APIs have handler/database acceptance, but source explorer and graph controls have no delivered visual workflow yet. The future browser checklist will cover source-only work without synthetic Issues, source rename/link/unlink, parallel A/B with gated C, addressed mail/processed receipts, same-session input, Hold/Cancel/restart, permissions and partial outages once those workflows exist. Do not count these future checks as passed or ask the user to test missing controls.

## Publication boundary

Verified milestones are published with jj to `origin` (`smdex/multica`) on `main`, with a dry-run and fast-forward ancestry check. Unfinished worker files and unrelated local edits stay local. Private agent transcripts are excluded from the published milestone ancestry. The original local history and working copy are preserved.
