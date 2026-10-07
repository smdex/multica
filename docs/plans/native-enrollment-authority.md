# Native enrollment authority milestone

Status: the backend authority slice is verified. Local provisioning and daemon operational enrollment remain under acceptance. This is not full swarm completion.

The same operator must be both the current workspace owner/admin and the exact non-null runtime owner. A fresh enrollment creates a disabled native source. Existing observe sources cannot be adopted or upgraded. Enrollment does not initialize Beads, enable a source, launch an agent or start a graph.

| Interface | Contract |
| --- | --- |
| `POST /api/daemon/runtimes/{runtimeId}/source-enrollments` | Human JWT/PAT, `X-Workspace-ID`, and `{request_id,name}`. New intent returns 201; an identical retry returns the same source and enrollment nonce with 200. Changed inputs return 409. |
| `POST /api/daemon/runtimes/{runtimeId}/source-enrollments/{sourceId}/token` | Fresh human authority and `{enrollment_id,config_revision,manifest_hash}`. First approved hash is immutable. Returns a no-store, at-most-120-second `mse_` bearer, clipped to parent expiry. |
| `POST /api/daemon/runtimes/{runtimeId}/source-enrollments/{sourceId}/finalize` | Only that scoped capability and the identical proof. Current ownership, membership incarnation, runtime incarnation, parent PAT, source revision and expiry are checked again inside the transaction, including after lock waits and before terminal replay. |

## Observed checks

The exact backend commit `5018e738f771cf5e6f3823d8e5f3528c2afed168` passed all six production-router/PostgreSQL enrollment tests under race detection three times on October 7, 2026. The isolated archive contained no unfinished CLI, local domain manager or UI files.

| Requirement | Check | Observed outcome |
| --- | --- | --- |
| Fresh scoped intent, durable identical-request replay and strict validation | `TestNativeSourceEnrollmentIntentAuthority` | 201 new, 200 same source/enrollment replay, 409 changed input and pending Enable, 403 unauthorized ownership, 404 wrong workspace, 400 malformed input. |
| Narrow approval capability and live parent authority | `TestNativeSourceEnrollmentIssuanceAndRevocation` | Exact scope/selectors, no-store, bounded lifetime, immutable hash and fenced routes. Actual PAT revocation invalidated issuance and the child capability. |
| Scoped finalization, terminal replay and deletion | `TestNativeSourceEnrollmentFinalize` | Human/read capabilities refused; matching enrollment finalized and replayed with 200. Source DELETE returned 204; old capability then returned 404. |
| Expiry rechecked after a real source row-lock wait | `TestNativeSourceEnrollmentExpiryAfterSourceLockWait` | `pg_blocking_pids` observed blocking. Expired pending finalization and expired terminal replay returned 401 without changing pending/enrolled state. |
| Membership removal and re-add cannot rehabilitate authority | `TestNativeSourceEnrollmentMemberIncarnationFence` | Actual member DELETE returned 204; old grant returned 404. Re-add created a different member UUID, and old finalization and fresh issuance for the old enrollment returned 403. |
| Role, runtime incarnation and configuration pins | `TestNativeSourceEnrollmentAuthorityPinsAfterIssuance` | Demotion and changed runtime creation time returned 403. Renaming changed the revision, old capability returned 409, and a fresh approval finalized the same enrollment at the new revision. Agent/Issue/task counts remained unchanged. |

Token checks, backend vet and server build passed. Existing source-read credential, registration and draft HTTP workflows passed separately against this immutable backend tree. Concurrent-index migration cleanup checks passed. No browser, mobile or signed-installer checks ran.

Reviewed migrations 600 and 601 add enrollment pins and a concurrent unique workspace/request index. They were applied individually through the production runner. No blanket implementation migration run occurred. Down migrations were inspected, not executed against retained development data.

## Reproduce nonvisual acceptance

Use your checkout's existing managed database and review pending migrations before applying them. Do not reset passwords or create an assumed database.

```bash
devenv shell -- bash -c 'cd server && go test -race ./internal/auth -run "Source(Read|Enrollment)Token" -count=1'
devenv shell -- bash -c 'cd server && go test -race ./cmd/server -run "^TestNativeSourceEnrollment(IntentAuthority|IssuanceAndRevocation|Finalize|ExpiryAfterSourceLockWait|MemberIncarnationFence|AuthorityPinsAfterIssuance)$" -count=3 -v'
```

These tests use fixture-owned runtimes and credentials, not installed agents. Never record capability response bodies in logs or screenshots.

## Trust and delivery limits

The server authenticates an approved operator and immutable identity pins, not remote filesystem state or exclusion of arbitrary external programs. A bearer holder can perform scoped finalization. Enrollment is not cryptographic proof that a daemon holds an OS lock. Protected local deployment and the cooperating owner-domain model remain prerequisites for the local operational slice.

The local provisioning CLI and daemon enrollment loop are not part of this backend milestone. They remain under operational acceptance. Graph execution, typed handoffs/mail, session controls, restart recovery and jj execution lifecycle remain unfinished. User-owned visual instructions for existing screens remain in [the milestone guide](native-swarm-milestones.md).
