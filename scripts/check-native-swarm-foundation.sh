#!/usr/bin/env bash
set -euo pipefail

# Run through the checkout's managed shell. This check does not start services,
# apply migrations, resolve real agent CLIs, or contact provider accounts.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "DATABASE_URL must select this checkout's managed database." >&2
  exit 1
fi

scratch="${JCODE_SCRATCH_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/multica-verification}"
mkdir -p "$scratch"
log="$(mktemp "$scratch/native-swarm-foundation.XXXXXX")"
trap 'rm -f "$log"' EXIT

run_checked() {
  local expected_tests="$1"
  shift
  : > "$log"
  if ! "$@" > "$log" 2>&1; then
    cat "$log"
    return 1
  fi
  cat "$log"
  if grep -Eq 'Skipping tests:|--- SKIP:|\[no test files\]' "$log"; then
    echo "A required check was skipped. This is not a successful verification." >&2
    return 1
  fi
  local expected_test
  # Test names contain no whitespace. Require each named acceptance check.
  for expected_test in $expected_tests; do
    if ! grep -Fq -- "--- PASS: $expected_test " "$log"; then
      echo "Expected acceptance check did not execute: $expected_test" >&2
      return 1
    fi
  done
}

cd server
run_checked 'TestReportTaskMessagesIdentifiedBatchReplay TestReportTaskMessagesIdentifiedBatchConcurrentReplay TestReportTaskMessagesLostReceiptThroughRealClient TestTaskMessageBatchIdentityUsesDecodedFields' \
  go test -race ./internal/handler \
  -run '^Test(ReportTaskMessages|CreateTaskMessagesBatchIsAtomic|TaskMessageCapabilities|TaskMessageBatchIdentity)' -count=1 -v
run_checked 'TestEveryConcurrentUpBuildHasCleanup TestEveryConcurrentDownBuildHasCleanup' \
  go test ./cmd/migrate -run '^TestEveryConcurrent(Up|Down)BuildHasCleanup$' -count=1 -v
run_checked TestWorkflowRunSteersTheBoundSessionWithReplaySafety \
  go test -race ./internal/daemon \
  -run '^TestWorkflowRunSteersTheBoundSessionWithReplaySafety$' -count=1 -v

run_checked 'TestWorkSourceManyToManyLinks TestWorkSourceLifecycle TestWorkSourceHandlerGuards TestWorkSourceDeleteLinkRace TestWorkSourceRuntimeTeardownRefusal TestWorkSourceRuntimeDeleteEndpoint409 TestWorkSourceParentDeletion TestWorkSourceRuntimeMergeRefusal' \
  go test -race ./internal/handler -run '^TestWorkSource(ManyToManyLinks|Lifecycle|HandlerGuards|DeleteLinkRace|RuntimeTeardownRefusal|RuntimeDeleteEndpoint409|ParentDeletion|RuntimeMergeRefusal)$' -count=1 -timeout 120s -v
run_checked 'TestWorkSourceCommandCreateAllowlistAndGuards TestWorkSourceCommandOwnerRoutedClaimReport TestWorkSourceCommandRevisionDisabledAndExpired TestWorkSourceCommandWorkspaceRuntimeLockOrder TestWorkSourceCommandSourceDeletion TestWorkSourceCommandWorkspaceDeletion TestWorkspaceDeletionManifestCoversPublicSchema' \
  go test -race ./internal/handler -run '^Test(WorkSourceCommand|WorkspaceDeletionManifestCoversPublicSchema)' -count=1 -timeout 120s -v
run_checked 'TestValidateWorkSourceCommandReport TestWorkSourceCommandCanonicalResult' \
  go test -race ./internal/service -run '^Test(ValidateWorkSourceCommandReport|WorkSourceCommandCanonicalResult)$' -count=1 -v
run_checked 'TestWorkSourceCommandReceiptsThroughRouter TestSweepExpiredWorkSourceCommands' \
  go test -race ./cmd/server -run '^Test(WorkSourceCommandReceiptsThroughRouter|SweepExpiredWorkSourceCommands)$' -count=1 -timeout 120s -v
run_checked 'TestRuntimeGC_KeepsWorkSourceOwner TestRuntimeGC_KeepsTerminalTaskHistory' \
  go test -race ./cmd/server -run '^TestRuntimeGC' -count=1 -timeout 120s -v

printf '\nReceipt, authenticated source-command transport, source lifecycle and same-session interface foundations verified. Automatic source dispatch and full swarm E2E remain separate acceptance.\n'
