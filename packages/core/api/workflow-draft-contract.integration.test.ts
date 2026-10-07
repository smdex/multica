// @vitest-environment node
import { afterAll, describe, expect, it } from "vitest";
import { ApiClient, ApiError } from "./client";

/**
 * Cross-language workflow-draft contract acceptance. Invoked by
 * server/cmd/server/workflow_draft_frontend_contract_test.go, which prepares
 * A/B->C terminal read receipts on the Go production router (testServer +
 * PostgreSQL) and passes selectors via MULTICA_DRAFT_CONTRACT_* env vars.
 * This file consumes them through the REAL ApiClient
 * createWorkflowDraft/getWorkflowDraft with no fetch mocks or type casts.
 *
 * Ordinary `pnpm test` skips. When MULTICA_DRAFT_CONTRACT_RUN=1 is set but
 * any selector is missing, the suite FAILS instead of silently skipping.
 * Credentials are read from env only and never printed.
 */

const RUN = process.env.MULTICA_DRAFT_CONTRACT_RUN === "1";
const BASE_URL = process.env.MULTICA_DRAFT_CONTRACT_BASE_URL ?? "";
const TOKEN = process.env.MULTICA_DRAFT_CONTRACT_TOKEN ?? "";
const WS_ID = process.env.MULTICA_DRAFT_CONTRACT_WORKSPACE ?? "";
const SOURCE_ID = process.env.MULTICA_DRAFT_CONTRACT_SOURCE_ID ?? "";
const REQUEST_ID = process.env.MULTICA_DRAFT_CONTRACT_REQUEST_ID ?? "";
const CONFIG_REVISION = process.env.MULTICA_DRAFT_CONTRACT_CONFIG_REVISION ?? "";
const CAPACITY = process.env.MULTICA_DRAFT_CONTRACT_CAPACITY ?? "";
const ready = [BASE_URL, TOKEN, WS_ID, SOURCE_ID, REQUEST_ID, CONFIG_REVISION, CAPACITY].every((v) => v !== "");

if (RUN && !ready) {
  throw new Error(
    "MULTICA_DRAFT_CONTRACT_RUN=1 requires every MULTICA_DRAFT_CONTRACT_* selector: BASE_URL, TOKEN, WORKSPACE, SOURCE_ID, REQUEST_ID, CONFIG_REVISION, CAPACITY",
  );
}

describe.skipIf(!RUN)("workflow draft contract (integration, Go production server)", () => {
  const wsId = WS_ID;
  const sourceId = SOURCE_ID;
  const configRevision = Number(CONFIG_REVISION);
  const capacity = Number(CAPACITY);
  const rootNativeId = "wfd-contract-root";
  const rootRevision = "rev-wfd-contract-root";
  // Receipt ids are fixed server-side; order intentionally differs per call:
  // the retry leg exercises a reordered-but-identical receipt_ids array.
  const receiptsCanonical = process.env.MULTICA_DRAFT_CONTRACT_RECEIPTS?.split(",") ?? [];
  if (RUN && receiptsCanonical.length !== 3) {
    throw new Error("MULTICA_DRAFT_CONTRACT_RECEIPTS must list exactly three receipt UUIDs");
  }
  const receiptsReordered = receiptsCanonical.slice().reverse();
  // Set by the create test; the retry test must prove idempotence against the
  // exact stored draft identity, not merely matching selectors.
  let createdDraftId = "";

  const client = new ApiClient(BASE_URL);
  client.setToken(TOKEN);

  afterAll(() => client.setToken(null));

  it("create parses a new frozen draft with a closed graph and draft-blocked node_state", async () => {
    const draft = await client.createWorkflowDraft({
      workspaceUuid: wsId,
      body: {
        request_id: REQUEST_ID,
        source_id: sourceId,
        root_native_id: rootNativeId,
        expected_root_revision: rootRevision,
        expected_config_revision: configRevision,
        capacity,
        receipt_ids: receiptsCanonical,
      },
    });
    expect(draft.workspace_id).toBe(wsId);
    expect(draft.source_id).toBe(sourceId);
    expect(draft.request_id).toBe(REQUEST_ID);
    expect(draft.root_native_id).toBe(rootNativeId);
    expect(draft.config_revision).toBe(configRevision);
    expect(draft.capacity).toBe(capacity);
    expect(draft.status).toBe("draft");
    createdDraftId = draft.id;

    const read = await client.getWorkflowDraft({ workspaceUuid: wsId, draftId: draft.id });
    expect(read.id).toBe(draft.id);
    expect(read.graph.workspace_id).toBe(wsId);
    expect(read.graph.source_id).toBe(sourceId);
    expect(read.graph.root_native_id).toBe(rootNativeId);
    expect(read.graph.config_revision).toBe(configRevision);
    expect(read.graph.nodes).toHaveLength(3);
    const ids = read.graph.nodes.map((n) => n.native_id).sort();
    expect(ids).toEqual([rootNativeId, "wfd-contract-a", "wfd-contract-b"].sort());
    for (const node of read.graph.nodes) {
      expect(node.revision).toBe("rev-" + node.native_id);
      expect(node.title).toBe("title-" + node.native_id);
      expect(node.receipt_id).toMatch(/^[0-9a-f-]{36}$/i);
      expect(node.observed_at).not.toBe("");
    }
    expect(read.graph.edges).toHaveLength(2);
    for (const edge of read.graph.edges) {
      expect(edge.dependency_type).toBe("blocks");
      expect(edge.consumer_native_id).toBe(rootNativeId);
      expect(["wfd-contract-a", "wfd-contract-b"]).toContain(edge.predecessor_native_id);
    }
    expect(read.graph.digest).toMatch(/^[0-9a-f]{64}$/);
    expect(Object.keys(read.node_state).sort()).toEqual(ids);
    for (const state of Object.values(read.node_state)) {
      expect(state).toEqual({ status: "blocked", reason: "draft" });
    }
  });

  it("identical retry with reordered receipts returns the same draft", async () => {
    const retry = await client.createWorkflowDraft({
      workspaceUuid: wsId,
      body: {
        request_id: REQUEST_ID,
        source_id: sourceId,
        root_native_id: rootNativeId,
        expected_root_revision: rootRevision,
        expected_config_revision: configRevision,
        capacity,
        receipt_ids: receiptsReordered,
      },
    });
    // Same request UUID, identical closure: the server returns the stored
    // draft (200) parsed identically to the original create.
    if (createdDraftId === "") throw new Error("create test must run first and record the draft id");
    expect(retry.id).toBe(createdDraftId);
    expect(retry.root_native_id).toBe(rootNativeId);
    expect(retry.capacity).toBe(capacity);
    expect(retry.graph.nodes).toHaveLength(3);
    expect(retry.graph.edges).toHaveLength(2);
    expect(Object.keys(retry.node_state)).toHaveLength(3);
  });

  it("same request UUID with changed capacity is rejected with 409", async () => {
    const err: unknown = await client
      .createWorkflowDraft({
        workspaceUuid: wsId,
        body: {
          request_id: REQUEST_ID,
          source_id: sourceId,
          root_native_id: rootNativeId,
          expected_root_revision: rootRevision,
          expected_config_revision: configRevision,
          capacity: capacity === 1 ? 2 : 1,
          receipt_ids: receiptsCanonical,
        },
      })
      .catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
  });
});
