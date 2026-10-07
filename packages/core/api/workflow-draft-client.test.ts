// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient, ApiError } from "./client";
import { setCurrentWorkspace } from "../platform";

const digest = "a".repeat(64);

const graph = {
  workspace_id: "ws-1",
  source_id: "src-1",
  root_native_id: "beads-1",
  config_revision: 3,
  nodes: [
    {
      native_id: "beads-1",
      revision: "rev-1",
      title: "Root",
      status: "open",
      receipt_id: "11111111-1111-1111-1111-111111111111",
      observed_at: "2026-10-07T00:00:00Z",
    },
    {
      native_id: "beads-2",
      revision: "rev-2",
      title: "Child",
      status: "open",
      receipt_id: "22222222-2222-2222-2222-222222222222",
      observed_at: "2026-10-07T00:00:00Z",
    },
  ],
  edges: [
    { predecessor_native_id: "beads-2", consumer_native_id: "beads-1", dependency_type: "blocks" },
  ],
  digest,
};

const draft = {
  id: "run-1",
  workspace_id: "ws-1",
  source_id: "src-1",
  request_id: "33333333-3333-3333-3333-333333333333",
  root_native_id: "beads-1",
  config_revision: 3,
  capacity: 1,
  status: "draft",
  graph,
  node_state: {
    "beads-1": { status: "blocked", reason: "draft" },
    "beads-2": { status: "blocked", reason: "draft" },
  },
  created_by: "user-1",
  created_at: "2026-10-07T00:00:00Z",
};

const createBody = {
  request_id: draft.request_id,
  source_id: "src-1",
  root_native_id: "beads-1",
  expected_root_revision: "rev-1",
  expected_config_revision: 3,
  capacity: 1,
  receipt_ids: [draft.request_id],
};

function stubFetch(body: unknown, status = 200): ReturnType<typeof vi.fn> {
  const fn = vi.fn().mockResolvedValue(
    new Response(status === 204 ? null : JSON.stringify(body), { status }),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
  setCurrentWorkspace(null, null);
});

describe("workflow draft client", () => {
  it("creates a draft on 201 and parses the frozen graph", async () => {
    stubFetch(draft, 201);
    const client = new ApiClient("https://api.example.test");
    const created = await client.createWorkflowDraft({ workspaceUuid: "ws-1", body: createBody });
    expect(created.id).toBe("run-1");
    expect(created.graph.nodes).toHaveLength(2);
    expect(created.node_state["beads-1"]).toEqual({ status: "blocked", reason: "draft" });
  });

  it("preserves request_id on an idempotent 200 retry", async () => {
    stubFetch(draft, 200);
    const client = new ApiClient("https://api.example.test");
    const retried = await client.createWorkflowDraft({ workspaceUuid: "ws-1", body: createBody });
    expect(retried.request_id).toBe(draft.request_id);
    expect(retried.id).toBe("run-1");
  });

  it("gets a draft by id", async () => {
    stubFetch(draft);
    const client = new ApiClient("https://api.example.test");
    const got = await client.getWorkflowDraft({ workspaceUuid: "ws-1", draftId: "run-1" });
    expect(got.graph.digest).toBe(digest);
  });

  it.each([
    ["node_state status", { ...draft, node_state: { "beads-1": { status: "ready", reason: "draft" }, "beads-2": { status: "blocked", reason: "draft" } } }],
    ["node_state reason", { ...draft, node_state: { "beads-1": { status: "blocked", reason: "approved" }, "beads-2": { status: "blocked", reason: "draft" } } }],
    ["null graph", { ...draft, graph: null }],
    ["missing nodes", { ...draft, graph: { ...graph, nodes: [] } }],
    ["missing node_state entry", { ...draft, node_state: { "beads-1": { status: "blocked", reason: "draft" } } }],
    ["node_state names an unknown node", { ...draft, node_state: { ...draft.node_state, "beads-9": { status: "blocked", reason: "draft" } } }],
    ["graph scope mismatch", { ...draft, graph: { ...graph, workspace_id: "ws-2" } }],
    ["graph root mismatch", { ...draft, graph: { ...graph, root_native_id: "beads-7" } }],
    ["graph config mismatch", { ...draft, graph: { ...graph, config_revision: 4 } }],
    ["bad capacity", { ...draft, capacity: 3 }],
    ["zero capacity", { ...draft, capacity: 0 }],
    ["bad edge kind", { ...draft, graph: { ...graph, edges: [{ predecessor_native_id: "beads-2", consumer_native_id: "beads-1", dependency_type: "relates" }] } }],
    ["edge references unknown node", { ...draft, graph: { ...graph, edges: [{ predecessor_native_id: "beads-9", consumer_native_id: "beads-1", dependency_type: "blocks" }] } }],
    ["duplicate node identity", { ...draft, graph: { ...graph, nodes: [...graph.nodes, { ...graph.nodes[0] }] } }],
    ["digest not sha256 hex", { ...draft, graph: { ...graph, digest: "deadbeef" } }],
    ["node bound exceeded", { ...draft, graph: { ...graph, nodes: Array.from({ length: 129 }, (_, i) => ({ ...graph.nodes[0], native_id: `beads-${i}` })) } }],
    ["missing request_id", { ...draft, request_id: "" }],
    ["malformed run status", { ...draft, status: "running" }],
    ["null run status", { ...draft, status: null }],
    ["zero outer config_revision", { ...draft, config_revision: 0, graph: { ...graph, config_revision: 0 } }],
    ["zero graph config_revision", { ...draft, graph: { ...graph, config_revision: 0 } }],
    ["empty nodes array", { ...draft, graph: { ...graph, nodes: [] }, node_state: {} }],
    ["root missing from nodes", { ...draft, graph: { ...graph, nodes: [graph.nodes[1]] }, node_state: { "beads-2": { status: "blocked", reason: "draft" } } }],
    ["whitespace-only native_id", { ...draft, graph: { ...graph, nodes: [graph.nodes[0], { ...graph.nodes[1], native_id: "   " }] }, node_state: { ...draft.node_state, "   ": { status: "blocked", reason: "draft" } } }],
    ["whitespace-only revision", { ...draft, graph: { ...graph, nodes: [graph.nodes[0], { ...graph.nodes[1], revision: "\t" }] } }],
    ["whitespace-only graph root_native_id", { ...draft, graph: { ...graph, root_native_id: " " } }],
  ])("rejects a malformed draft: %s", async (_label, body) => {
    stubFetch(body);
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.getWorkflowDraft({ workspaceUuid: "ws-1", draftId: "run-1" }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("clears the stale global workspace slug on both draft calls", async () => {
    setCurrentWorkspace("other-workspace", "ws-other");
    const fn = stubFetch(draft);
    const client = new ApiClient("https://api.example.test");
    await client.createWorkflowDraft({ workspaceUuid: "ws-1", body: createBody });
    const headers = new Headers(fn.mock.calls[0]?.[1]?.headers);
    expect(headers.get("X-Workspace-ID")).toBe("ws-1");
    expect(headers.get("X-Workspace-Slug")).toBe("");
  });

  it("surfaces 404 and 409 as ApiError without parsing", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch({ error: "not found" }, 404);
    await expect(
      client.getWorkflowDraft({ workspaceUuid: "ws-1", draftId: "run-1" }),
    ).rejects.toBeInstanceOf(ApiError);
    stubFetch({ error: "precondition changed" }, 409);
    const rejection = client.createWorkflowDraft({ workspaceUuid: "ws-1", body: createBody });
    await expect(rejection).rejects.toBeInstanceOf(ApiError);
    await expect(rejection).rejects.toMatchObject({ status: 409 });
  });
});
