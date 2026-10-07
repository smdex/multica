// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { CreateWorkSourceCommandParams, WorkSourceCommand } from "../types/work-source";
import { assertWorkSourceReadReceipt, sourceReadPollInterval, workSourceReadOptions } from "./reads";
import { workSourcesOptions, workSourceCommandsOptions } from "./queries";

vi.mock("../api", () => ({ api: { getWorkSourceCommand: vi.fn(), listWorkSources: vi.fn(), listWorkSourceCommands: vi.fn() } }));

const request: CreateWorkSourceCommandParams = {
  request_id: "ad454705-a258-4b6c-8197-3e962f87fa52", command: "list", limit: 50,
};
const pending: WorkSourceCommand = {
  id: "da3e1098-c78a-4386-96af-439b21b1ebbb", request_id: request.request_id,
  workspace_id: "ws", source_id: "source", command: "list", limit_count: 50,
  status: "pending", config_revision: 1, expires_at: "2026-10-07T17:00:00Z",
  created_at: "2026-10-07T16:55:00Z", updated_at: "2026-10-07T16:55:00Z",
};
beforeEach(() => vi.clearAllMocks());

describe("source read identity", () => {
  it("accepts the exact intent and the server's default list bound", () => {
    expect(assertWorkSourceReadReceipt(pending, "ws", "source", request, pending.id)).toBe(pending);
    const defaultReceipt = { ...pending, limit_count: undefined };
    expect(assertWorkSourceReadReceipt(defaultReceipt, "ws", "source", { ...request, limit: undefined })).toBe(defaultReceipt);
    expect(() => assertWorkSourceReadReceipt(pending, "ws", "source", { ...request, limit: undefined })).toThrow();
  });

  it.each([
    { workspace_id: "foreign" }, { source_id: "foreign" }, { request_id: "foreign" },
    { id: "foreign" }, { command: "read" as const }, { limit_count: 49 }, { native_id: "foreign" },
  ])("rejects a changed receipt selector %j", (patch) => {
    expect(() => assertWorkSourceReadReceipt({ ...pending, ...patch }, "ws", "source", request, pending.id)).toThrow(/does not match/);
  });

  it("preserves opaque detail identity and rejects its list field", () => {
    const read = { ...request, command: "read" as const, limit: undefined, native_id: " bd-1 " };
    const receipt = { ...pending, command: "read" as const, limit_count: undefined, native_id: " bd-1 " };
    expect(assertWorkSourceReadReceipt(receipt, "ws", "source", read)).toBe(receipt);
    expect(() => assertWorkSourceReadReceipt({ ...receipt, native_id: "bd-1" }, "ws", "source", read)).toThrow();
    expect(() => assertWorkSourceReadReceipt({ ...receipt, limit_count: 1 }, "ws", "source", read)).toThrow();
  });
});

describe("source receipt query", () => {
  it("does not cache source metadata from another workspace", async () => {
    vi.mocked(api.listWorkSources).mockResolvedValue([{
      id: "source", workspace_id: "foreign", runtime_id: "runtime", daemon_id: "daemon",
      name: "Foreign source", mode: "observe", enabled: true, source_handle: "handle",
      config_revision: 1, created_at: pending.created_at, updated_at: pending.updated_at,
    }]);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const options = workSourcesOptions("ws");
    await expect(qc.fetchQuery(options)).rejects.toThrow(/workspace/);
    expect(qc.getQueryData(options.queryKey)).toBeUndefined();
    qc.clear();
  });

  it.each([{ workspace_id: "foreign" }, { source_id: "foreign" }])("does not cache out-of-scope history %j", async (patch) => {
    vi.mocked(api.listWorkSourceCommands).mockResolvedValue([{ ...pending, ...patch }]);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const options = workSourceCommandsOptions("ws", "source");
    await expect(qc.fetchQuery(options)).rejects.toThrow(/scope/);
    expect(qc.getQueryData(options.queryKey)).toBeUndefined();
    qc.clear();
  });

  it("accepts history belonging to the exact requested scope", async () => {
    vi.mocked(api.listWorkSourceCommands).mockResolvedValue([pending]);
    const qc = new QueryClient();
    const options = workSourceCommandsOptions("ws", "source");
    expect(await qc.fetchQuery(options)).toEqual([pending]);
    qc.clear();
  });
  it("pins workspace/command and passes cancellation to the actual API boundary", async () => {
    vi.mocked(api.getWorkSourceCommand).mockResolvedValue(pending);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const options = workSourceReadOptions("ws", "source", pending.id, request);
    expect(options.queryKey).toEqual(["work-source-commands", "ws", "detail", pending.id]);
    expect(options.enabled).toBe(true);
    expect(await qc.fetchQuery(options)).toEqual(pending);
    expect(api.getWorkSourceCommand).toHaveBeenCalledWith({ workspaceUuid: "ws", commandId: pending.id, signal: expect.any(AbortSignal) });
    qc.clear();
  });

  it("never caches a fetched cross-source receipt", async () => {
    vi.mocked(api.getWorkSourceCommand).mockResolvedValue({ ...pending, source_id: "foreign" });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const options = workSourceReadOptions("ws", "source", pending.id, request);
    await expect(qc.fetchQuery(options)).rejects.toThrow(/does not match/);
    expect(qc.getQueryData(options.queryKey)).toBeUndefined();
    qc.clear();
  });

  it("does not enable polling without scope, intent or receipt identity", () => {
    expect(workSourceReadOptions("", "source", pending.id, request).enabled).toBe(false);
    expect(workSourceReadOptions("ws", "", pending.id, request).enabled).toBe(false);
    expect(workSourceReadOptions("ws", "source", "", request).enabled).toBe(false);
    expect(workSourceReadOptions("ws", "source", pending.id, undefined).enabled).toBe(false);
  });

  it("polls only unexpired in-flight receipts and stops exactly at expiry", () => {
    const expiry = Date.parse(pending.expires_at);
    expect(sourceReadPollInterval(pending, expiry - 1)).toBe(2000);
    expect(sourceReadPollInterval({ ...pending, status: "claimed" }, expiry - 1)).toBe(2000);
    expect(sourceReadPollInterval({ ...pending, status: "unknown" }, expiry - 1)).toBe(2000);
    expect(sourceReadPollInterval({ ...pending, status: "unknown" }, expiry)).toBe(false);
    expect(sourceReadPollInterval(pending, expiry)).toBe(false);
    expect(sourceReadPollInterval({ ...pending, status: "succeeded" }, expiry - 1)).toBe(false);
    expect(sourceReadPollInterval({ ...pending, status: "failed" }, expiry - 1)).toBe(false);
    expect(sourceReadPollInterval({ ...pending, expires_at: "invalid" })).toBe(false);
    expect(sourceReadPollInterval(undefined)).toBe(false);
  });
});
