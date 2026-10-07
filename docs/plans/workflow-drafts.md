# Nonexecuting workflow drafts

This slice saves a frozen, bounded observation set. It does not enqueue agents, create Issues, grant source ownership, change observe mode or enable Start. A draft is not an atomic source snapshot and its digest is not source revision CAS.

## Public contract

Workspace owners/admins submit `POST /api/workflow-runs`, using the workspace UUID header:

```json
{
  "request_id": "<fresh UUID retained for identical retries>",
  "source_id": "<workspace source UUID>",
  "root_native_id": "<opaque native ID>",
  "expected_root_revision": "<opaque revision from the root receipt>",
  "expected_config_revision": 1,
  "capacity": 2,
  "receipt_ids": ["<succeeded detail receipt UUID>"]
}
```

The server, not the browser, derives the graph from stored complete typed `read` receipts. Every predecessor recursively reachable from the root requires a same-workspace/source/configuration receipt. Only qualified `blocks` edges are supported. Missing observations, cycles, incomplete legacy receipts, unsupported types and explicit external boundaries are rejected, not silently removed. Extra valid receipt IDs cannot expand the root closure. Multiple observations of the same native ID are ambiguous and rejected. Limits are 128 supplied receipts/nodes, 512 edges, capacity 1..2 and 2 MiB of aggregate detail evidence and final graph JSON, including its digest.

A new draft returns 201. Identical workspace-scoped request UUID/body retries return the existing row with 200, including after source disablement. Changed source, receipts, capacity or preconditions with that UUID return 409. Receipt order does not change request identity. The identity is retained while the draft exists. Source/workspace deletion removes it together with the draft, not a permanent tombstone. New requests require the current enabled source and configuration revision. Invalid input is 400, missing scoped resources 404 and unsupported/incomplete closure 422.

`GET /api/workflow-runs/{id}` is available to workspace members. The response includes `status: "draft"`, scoped source/root/configuration identities, sorted frozen nodes and typed predecessor-to-consumer edges, copied receipt UUIDs/acceptance timestamps and the observation-set SHA256 digest. Initial `node_state` is blocked with reason draft for every frozen native ID. There is no execution endpoint in this slice.

## Persistence and deletion

Graph and node state are inserted atomically in one `workflow_run` row. There are no graph node/edge tables, queue attempts or foreign keys. Table migration 597 and the independently concurrent ID/request indexes 598/599 are idempotent.

Creation serializes live creator membership, then locks workspace, request identity, optional project and source before loading receipts/inserting. Source and project deletion also take the workspace parent lock before descendants. Source cascade and project detachment lock sources before workflow rows. Source deletion removes drafts and receipts in the same source-locked transaction. Project deletion detaches the run's project context alongside the source; the frozen observation graph remains unchanged. Workspace deletion sweeps drafts atomically with source teardown. Copied provenance does not depend on later receipt retention.

## Acceptance boundary

Required nonvisual checks cover pure closure/canonicalization matrices, actual authenticated production-router/PostgreSQL admission/replay/permissions/deletion, malformed shared-client responses, concurrent delete/revoke fences and packaging. Passing these checks proves only this draft slice. Parallel execution and joins, mail/handoffs, Hold/Cancel, product session controls and restart recovery still require their own implementation and public-interface acceptance. Browser and visual testing remains user-owned.
