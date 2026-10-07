# Baseline validation before fusion implementation

Observed October 5, 2026. No product source had been changed for the fusion when these checks ran.

## Managed runtime

`devenv processes list` reported `api`, `daemon`, `postgres`, and `web` ready with zero restarts. Do not start another process/database manager. The existing daemon is a user process, not a disposable test worker: acceptance fixtures must not assign work to a user's real agent.

## Frontend typecheck

Command: `devenv shell -- pnpm typecheck`.

Result: failed, exit 2. Turbo reported 8 successful tasks of 10, with web as the failing task and desktop cancelled by the aggregate failure. Exact errors are TS2742 portable inferred-type errors for `useCases` / `useCasesSource`, referencing pnpm's internal Zod 4.3.6 paths:

- `apps/web/.source/index.ts:8`, generated output.
- `apps/web/lib/use-cases-source.ts:26`.
- `apps/web/source.config.ts:26`.

The fix must address the source/config/dependency typing boundary, not hand-edit generated `.source` files, suppress type checking globally, or use unsafe assertions. Add a scoped prerequisite repair node to the implementation plan and verify web plus full frontend typecheck after repair. Full command output is retained in the coordinator background-task record `095777tayw`; do not paste the whole log into agent prompts.

## Backend test safety

`make test` calls `scripts/ensure-postgres.sh`, applies migrations, then `scripts/test-go.sh --race`. Because this checkout uses devenv, the plan must avoid accidentally mixing the Docker-oriented service setup. The existing guarded test entry point can run through devenv after readiness/migration checks:

`devenv shell -- bash scripts/test-go.sh --race`

This is a planned command, not a passed check. `scripts/test-go.sh` wraps default agent tests with `go-test-with-agent-cli-guard.sh`. DB-backed packages may exit green while skipping their DB tests if the configured database is unavailable; acceptance must check the log for skips and verify the actual declared database rather than treating exit zero as proof.

## Backend race-suite result

Command attempted: `devenv shell -- bash scripts/test-go.sh --race`.

Result: failed, exit 1, before the separate guarded `pkg/agent` invocation. `TestDeleteWorkspacePluginDataClearsEveryPluginTable` in `server/internal/service/plugin_sql_integration_test.go:48` could not authenticate to the declared database (PostgreSQL SQLSTATE `28P01`). Other DB-aware packages can show `ok` after their TestMain skips DB fixtures, so those green package lines do not prove database coverage. The managed API uses the existing `secretspec run --provider keyring --profile development` wrapper, whereas the initial test used shell dotenv values. Retry with the same declared secret-resolution wrapper, without printing credentials or changing passwords. Preserve the failing baseline as evidence; it is not yet a product code defect.

## Browser harness

`playwright.config.ts` uses the existing running services, one Chromium worker, and does not auto-start servers. It loads `e2e/env.ts`; the selected URL must come from the declared checkout configuration, not guessed default ports. Test setup/cleanup must use `TestApiClient` and fixture-owned resources only. No browser E2E has run at this checkpoint.
