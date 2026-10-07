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

**Automatic source reads are not delivered yet.** Daemon-local approved handle mapping, pending-command dispatch and daemon credential bootstrap remain next steps. Creating a receipt does not execute `bd`: unclaimed reads expire to a visible failed receipt. The HTTP regression uses a test-owned daemon credential and typed test result, not a real source executor. Observe-mode sources cannot start agents or write Beads data. There is still no new source explorer or swarm UI to visually test.

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
