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
