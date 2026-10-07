// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient, ApiError } from "./client";
import { setCurrentWorkspace } from "../platform";

const source = {
  id: "src-1",
  workspace_id: "ws-1",
  runtime_id: "rt-1",
  daemon_id: "beads@host",
  name: "Beads main",
  mode: "observe",
  enabled: true,
  source_handle: "beads-main",
  config_revision: 1,
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
};

const link = {
  id: "link-1",
  workspace_id: "ws-1",
  issue_id: "issue-1",
  source_id: "src-1",
  native_id: "beads-42",
  created_at: "2026-10-07T00:00:00Z",
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

describe("work source client", () => {
  it.each([undefined, null, "invalid"])("defaults malformed capability fields safely (%s)", async (value) => {
    stubFetch([{ ...source, enabled: value, config_revision: value, mode: "future-mode" }]);
    const client = new ApiClient("https://api.example.test");
    const [parsed] = await client.listWorkSources({ workspaceUuid: "ws-1" });
    expect(parsed?.enabled).toBe(false);
    expect(parsed?.config_revision).toBe(0);
    expect(parsed?.mode).toBe("future-mode");
  });

  it("parses a well-formed source list", async () => {
    stubFetch([source]);
    const client = new ApiClient("https://api.example.test");
    const list = await client.listWorkSources({ workspaceUuid: "ws-1" });
    expect(list).toHaveLength(1);
    expect(list[0]?.source_handle).toBe("beads-main");
  });

  it("omits native enrollment fields for observe sources without them", async () => {
    stubFetch([source]);
    const client = new ApiClient("https://api.example.test");
    const [parsed] = await client.listWorkSources({ workspaceUuid: "ws-1" });
    expect(parsed?.native_enrollment_id).toBeUndefined();
    expect(parsed?.native_enrollment_status).toBeUndefined();
  });

  it("parses native enrollment fields and falls back unknown status to unknown", async () => {
    stubFetch([
      { ...source, mode: "native", native_enrollment_status: "enrolled", native_enrollment_id: "ne-1" },
      { ...source, id: "src-2", native_enrollment_status: "pending" },
      { ...source, id: "src-3", native_enrollment_status: "nonsense" },
      { ...source, id: "src-4", native_enrollment_status: null },
    ]);
    const client = new ApiClient("https://api.example.test");
    const list = await client.listWorkSources({ workspaceUuid: "ws-1" });
    expect(list[0]?.native_enrollment_status).toBe("enrolled");
    expect(list[1]?.native_enrollment_status).toBe("pending");
    // Unknown/legacy statuses fall back to "unknown", never "enrolled".
    expect(list[2]?.native_enrollment_status).toBe("unknown");
    expect(list[3]?.native_enrollment_status).toBe("unknown");
  });

  it("rejects a malformed source list instead of faking an empty one", async () => {
    stubFetch(null); // server sent JSON null
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.listWorkSources({ workspaceUuid: "ws-1" }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("rejects a source list entry missing its identity handle", async () => {
    stubFetch([{ ...source, source_handle: 42 }]);
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.listWorkSources({ workspaceUuid: "ws-1" }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("rejects a malformed create response instead of returning a phantom source", async () => {
    stubFetch({ error: "nope" }, 409);
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.createWorkSource({
        workspaceUuid: "ws-1",
        body: { runtime_id: "rt-1", name: "n", source_handle: "h" },
      }),
    ).rejects.toMatchObject({ status: 409 });
  });

  it("keeps update strict: a malformed body rejects", async () => {
    stubFetch("not-an-object");
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.updateWorkSource({
        workspaceUuid: "ws-1",
        sourceId: "src-1",
        body: { name: "renamed" },
      }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("delete returns void on 204 and surfaces non-2xx", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch(null, 204);
    await expect(
      client.deleteWorkSource({ workspaceUuid: "ws-1", sourceId: "src-1" }),
    ).resolves.toBeUndefined();
    stubFetch({ error: "still owns links" }, 409);
    const rejection = client.deleteWorkSource({ workspaceUuid: "ws-1", sourceId: "src-1" });
    await expect(rejection).rejects.toBeInstanceOf(ApiError);
    await expect(rejection).rejects.toMatchObject({ status: 409 });
  });

  it("parses links and rejects a malformed link list", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch([link]);
    const links = await client.listIssueWorkLinks({
      workspaceUuid: "ws-1",
      issueId: "issue-1",
    });
    expect(links[0]?.native_id).toBe("beads-42");
    stubFetch({ links: [link] }); // wrapped object, not a bare array
    await expect(
      client.listIssueWorkLinks({ workspaceUuid: "ws-1", issueId: "issue-1" }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("creates and deletes links through the strict path", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch(link, 201);
    const created = await client.createIssueWorkLink({
      workspaceUuid: "ws-1",
      body: { issue_id: "issue-1", source_id: "src-1", native_id: "beads-42" },
    });
    expect(created.id).toBe("link-1");
    stubFetch(null, 204);
    await expect(
      client.deleteIssueWorkLink({ workspaceUuid: "ws-1", linkId: "link-1" }),
    ).resolves.toBeUndefined();
  });

  it("clears the global workspace slug when a per-call UUID pins the request", async () => {
    // The user has switched to another workspace while this call is in flight
    // for ws-1. The server resolves X-Workspace-Slug BEFORE X-Workspace-ID,
    // so the slug must be cleared or the call silently targets the other ws.
    setCurrentWorkspace("other-workspace", "ws-other");
    const fn = stubFetch([source]);
    const client = new ApiClient("https://api.example.test");
    await client.listWorkSources({ workspaceUuid: "ws-1" });
    const headers = new Headers(fn.mock.calls[0]?.[1]?.headers);
    expect(headers.get("X-Workspace-ID")).toBe("ws-1");
    expect(headers.get("X-Workspace-Slug")).toBe("");
  });

  it("sends the filter params on the link list query", async () => {
    const fn = stubFetch([]);
    const client = new ApiClient("https://api.example.test");
    await client.listIssueWorkLinks({ workspaceUuid: "ws-1", sourceId: "src-1" });
    const url = String(fn.mock.calls[0]?.[0]);
    expect(url).toContain("source_id=src-1");
  });
});

const command = {
  request_id: "11111111-1111-1111-1111-111111111111",
  config_revision: 1,
  expires_at: "2026-10-08T00:00:00Z",
  id: "cmd-1",
  workspace_id: "ws-1",
  source_id: "src-1",
  command: "read",
  native_id: "beads-42",
  status: "pending",
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
};

describe("work source command client", () => {
  it("creates a command and preserves request_id on an idempotent retry", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch(command, 201);
    const body = { request_id: command.request_id, command: "read" as const, native_id: "beads-42" };
    const created = await client.createWorkSourceCommand({ workspaceUuid: "ws-1", sourceId: "src-1", body });
    expect(created.id).toBe("cmd-1");
    expect(created.status).toBe("pending");
    // Same request_id retry: server returns the stored receipt with 200.
    stubFetch({ ...command, status: "claimed" }, 200);
    const retried = await client.createWorkSourceCommand({ workspaceUuid: "ws-1", sourceId: "src-1", body });
    expect(retried.request_id).toBe(command.request_id);
    expect(retried.status).toBe("claimed");
  });

  it("gets a command and lists a source's commands", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch({ ...command, result: "{}" });
    const got = await client.getWorkSourceCommand({ workspaceUuid: "ws-1", commandId: "cmd-1" });
    expect(got.result).toBe("{}");
    stubFetch([command]);
    const list = await client.listWorkSourceCommands({ workspaceUuid: "ws-1", sourceId: "src-1" });
    expect(list[0]?.command).toBe("read");
    expect(list[0]?.result).toBeUndefined();
  });

  it.each([
    ["command", { ...command, command: "rm-rf" }],
    ["status", { ...command, status: null }],
    ["id", { ...command, id: null }],
    ["request_id", { ...command, request_id: "" }],
  ])("rejects a malformed command response: bad %s", async (_label, body) => {
    stubFetch(body);
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.getWorkSourceCommand({ workspaceUuid: "ws-1", commandId: "cmd-1" }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("keeps future string statuses unknown, never fabricating a terminal outcome", async () => {
    stubFetch({ ...command, status: "executing" });
    const client = new ApiClient("https://api.example.test");
    const got = await client.getWorkSourceCommand({ workspaceUuid: "ws-1", commandId: "cmd-1" });
    expect(got.status).toBe("unknown");
  });

  it("rejects a malformed command list instead of faking an empty one", async () => {
    stubFetch(null);
    const client = new ApiClient("https://api.example.test");
    await expect(
      client.listWorkSourceCommands({ workspaceUuid: "ws-1", sourceId: "src-1" }),
    ).rejects.toThrow(/Malformed response/);
  });

  it("defaults optional fields to undefined without masking identity", async () => {
    stubFetch({ ...command, native_id: null, limit_count: null, claimed_at: null });
    const client = new ApiClient("https://api.example.test");
    const got = await client.getWorkSourceCommand({ workspaceUuid: "ws-1", commandId: "cmd-1" });
    expect(got.native_id).toBeUndefined();
    expect(got.limit_count).toBeUndefined();
    expect(got.status).toBe("pending");
  });

  it("clears the stale global workspace slug on command calls", async () => {
    setCurrentWorkspace("other-workspace", "ws-other");
    const fn = stubFetch([command]);
    const client = new ApiClient("https://api.example.test");
    await client.listWorkSourceCommands({ workspaceUuid: "ws-1", sourceId: "src-1" });
    const headers = new Headers(fn.mock.calls[0]?.[1]?.headers);
    expect(headers.get("X-Workspace-ID")).toBe("ws-1");
    expect(headers.get("X-Workspace-Slug")).toBe("");
  });

  it("surfaces API errors as ApiError", async () => {
    const client = new ApiClient("https://api.example.test");
    stubFetch({ error: "another source command is in flight" }, 409);
    const rejection = client.createWorkSourceCommand({
      workspaceUuid: "ws-1",
      sourceId: "src-1",
      body: { request_id: command.request_id, command: "read", native_id: "beads-42" },
    });
    await expect(rejection).rejects.toBeInstanceOf(ApiError);
    await expect(rejection).rejects.toMatchObject({ status: 409 });
    stubFetch({ error: "not found" }, 404);
    await expect(
      client.getWorkSourceCommand({ workspaceUuid: "ws-1", commandId: "cmd-1" }),
    ).rejects.toMatchObject({ status: 404 });
  });
});
