# Optional fresh native source initialization

This is a Linux-only prerequisite, not full native swarm execution. The existing `daemon source provision` command accepts an explicit absolute, clean `--beads-executable` path. Without the flag its prior behavior is unchanged.

## Operator procedure

Use a disposable workspace and a current owner/admin account that also owns the exact online runtime. Before starting the daemon's source ownership loop, run:

```bash
multica --profile dev daemon source provision \
  --workspace '<workspace UUID>' --runtime '<owned runtime UUID>' \
  --name 'fresh native source' --request-id '<stable request UUID>' \
  --beads-executable '/absolute/path/to/qualified/bd'
```

The qualified version is Beads 1.3.1. The command creates its own fresh machine-global native domain, holds its original ownership lock, initializes before publishing the profile index, then releases the lock. It never adopts a project directory. Continue the ordinary daemon restart and approval procedure in [operational enrollment](native-enrollment-operational.md). Initialization leaves the server source pending and disabled.

An exact retry with the same request UUID, body and executable uses the matching private local `.native_initialized` receipt without reinitializing. If the daemon already holds the source lock, flagged replay refuses Busy. Unflagged provisioning retains its existing idempotent busy behavior. If initialization fails, the durable server intent may already exist. Keep the request UUID and investigate the partial domain. The command does not reset, delete, repair or adopt partial contents, and changing the UUID is not an automatic recovery strategy.

## Safety and qualification boundaries

- Linux execution pins the original opened directory descriptor for child cwd and inherits it as fd 3 for `BEADS_DIR` and operation-owned HOME. Replacing the source leaf cannot redirect initialization writes. Other platforms refuse without launching a process.
- Version and initialization use a closed environment. PATH contains only the explicit executable's directory. The selected Nix wrapper provides its own pinned Dolt dependency. User credentials, agent configuration, inherited Git configuration and Beads server variables are not passed to the child.
- Exact initialization arguments are `--sandbox init --non-interactive --quiet --backend dolt --prefix multica --skip-agents --skip-hooks`. No stealth, force, reset, hooks, remote setup or existing-database adoption is used.
- Telemetry opt-out variables are supplied. Network behavior was not firewalled or observed, so this does not claim network isolation.
- The local receipt records the qualified version, executable, ownership-marker hash and successful initialization time. It is not a content digest, dependency-completeness proof, process-tree quiescence claim or graph Start authorization. H remains the immutable ownership-marker digest.
- Subprocess deadlines kill the direct child and bound pipe waits. Surviving descendant death is not proven. Failed initialization leaves partial output rather than issuing destructive cleanup.
- No Issue, agent or queued graph task is synthesized. Typed graph handoffs, execution admission and restart recovery remain separate unfinished requirements.

## Nonvisual acceptance mapping

Task `762058mig2` passed the live candidate's race primitive matrix and all actual CLI/production-router/PostgreSQL initialization tests. Broader live task `849115arnp` passed full execenv/CLI packages, native read/enrollment race suites, vet/build and three public-workflow iterations.

Exact code commit `4dc370d9927c868abd0a25e85a12224c3b26fccf` then passed immutable archive acceptance in task `045575zy15`: full execenv/CLI race packages, daemon native/read regressions, vet and build, actual archive-built CLI plus production Daemon.Run/enrollment/initializer routes for three iterations, selected Beads 1.3.1 fresh initialization and receipt-preserving replay, scoped read credentials/receipt compatibility, help exposure, and Darwin compilation of the unsupported-platform wiring. The explicit acceptance runs had no skips. Independent source review found no blocking defect. Frontend/mobile code tests, browser/visual E2E and complete native swarm execution were not run for this backend/CLI-only slice.

| Requirement | Runnable check |
| --- | --- |
| Pinning, leaf replacement, strict receipts, version grammar/overflow, cancellation and no reset | `go test -race ./internal/daemon/execenv -run NativeInit -count=1` |
| Relative/unclean paths and managed-task refusal before intent | `go test -race ./cmd/multica -run NativeSourceCLIInitializer -count=1` |
| Actual CLI success, exact argv and phase-specific closed environment, first-index publication, same-request replay | `TestNativeSourceInitCLIAcceptance` |
| Partial failure refuses reinit, busy flagged refusal, existing unflagged replay | `TestNativeSourceInitCLICrashRefusesNonFreshRetry`, `TestNativeSourceInitCLIBusyLock` |
| Membership/admin/runtime ownership denials before local launch | `TestNativeSourceInitCLIAuthority` |
| Selected installed Beads through the actual CLI, private receipt and replay identity | `TestNativeSourceInitCLISelectedBeads`, explicitly gated below |
| Existing enrollment still uses the actual daemon and scoped routes | `TestNativeSourceEnrollmentDaemonRunAcceptance` |

Run from the checkout through its existing managed environment and authorized test database. This feature introduces no migration. Do not start another database manager or apply unrelated pending migrations.

```bash
go build -o "$PWD/multica-init-check" ./cmd/multica
MULTICA_NATIVE_ENROLLMENT_TEST_CLI="$PWD/multica-init-check" \
  go test -race ./cmd/server -run '^TestNativeSourceInitCLI(Acceptance|CrashRefusesNonFreshRetry|BusyLock|Authority)$' -count=3 -v
# Explicit operator qualification only, never part of default installed-CLI discovery:
MULTICA_NATIVE_ENROLLMENT_TEST_CLI="$PWD/multica-init-check" \
  MULTICA_NATIVE_INIT_TEST_BEADS='/absolute/path/to/qualified/bd' \
  go test -race ./cmd/server -run '^TestNativeSourceInitCLISelectedBeads$' -count=1 -v
```

The explicit CLI gate fails when its managed database is unavailable. Default tests use test-created executables. Browser and visual testing remain user-owned, with no new initialization UI or Start control added.
