// @vitest-environment node
import { QueryClient } from "@tanstack/react-query";
import { afterAll, describe, expect, it } from "vitest";
import { ApiClient } from "../api/client";
import { setApiInstance } from "../api";
import { parseWorkSourceReadResult } from "./read-result";
import { workSourceReadOptions } from "./reads";

/**
 * Cross-language source-read contract acceptance. Invoked by
 * server/cmd/server/source_read_frontend_contract_test.go, which prepares
 * terminal receipts on the Go production router (testServer + PostgreSQL)
 * and passes selectors via MULTICA_SOURCE_READ_CONTRACT_* env vars. This
 * file consumes them through the REAL ApiClient, the REAL
 * parseWorkSourceReadResult, and the REAL workSourceReadOptions query
 * pipeline, with no fetch mocks or type casts.
 *
 * Ordinary `pnpm test` skips. When MULTICA_SOURCE_READ_CONTRACT_RUN=1 is
 * set but any selector is missing, the suite FAILS instead of silently
 * skipping. Credentials are read from env only and never printed.
 */

const RUN = process.env.MULTICA_SOURCE_READ_CONTRACT_RUN === "1";
const BASE_URL = process.env.MULTICA_SOURCE_READ_CONTRACT_BASE_URL ?? "";
const TOKEN = process.env.MULTICA_SOURCE_READ_CONTRACT_TOKEN ?? "";
const WS_ID = process.env.MULTICA_SOURCE_READ_CONTRACT_WORKSPACE ?? "";
const SOURCE_ID = process.env.MULTICA_SOURCE_READ_CONTRACT_SOURCE_ID ?? "";
const LIST_COMMAND = process.env.MULTICA_SOURCE_READ_CONTRACT_LIST_COMMAND ?? "";
const LIST_REQUEST = process.env.MULTICA_SOURCE_READ_CONTRACT_LIST_REQUEST ?? "";
const READ_COMMAND = process.env.MULTICA_SOURCE_READ_CONTRACT_READ_COMMAND ?? "";
const DEFAULT_LIST_COMMAND = process.env.MULTICA_SOURCE_READ_CONTRACT_DEFAULT_LIST_COMMAND ?? "";
const DEFAULT_LIST_REQUEST = process.env.MULTICA_SOURCE_READ_CONTRACT_DEFAULT_LIST_REQUEST ?? "";
const ready = [
  BASE_URL,
  TOKEN,
  WS_ID,
  SOURCE_ID,
  LIST_COMMAND,
  LIST_REQUEST,
  READ_COMMAND,
  DEFAULT_LIST_COMMAND,
  DEFAULT_LIST_REQUEST,
].every((v) => v !== "");

if (RUN && !ready) {
  throw new Error(
    "MULTICA_SOURCE_READ_CONTRACT_RUN=1 requires every MULTICA_SOURCE_READ_CONTRACT_* selector: BASE_URL, TOKEN, WORKSPACE, SOURCE_ID, LIST_COMMAND, LIST_REQUEST, READ_COMMAND, DEFAULT_LIST_COMMAND, DEFAULT_LIST_REQUEST",
  );
}

describe.skipIf(!RUN)("work-source read contract (integration, Go production server)", () => {
  const wsId = WS_ID;
  const sourceId = SOURCE_ID;
  const client = new ApiClient(BASE_URL);
  client.setToken(TOKEN);
  setApiInstance(client);

  afterAll(() => client.setToken(null));

  it("lists work sources containing the contract source", async () => {
    const sources = await client.listWorkSources({ workspaceUuid: wsId });
    const hit = sources.find((s) => s.id === sourceId);
    expect(hit, "prepared source must appear in the production source list").toBeDefined();
    expect(hit!.workspace_id).toBe(wsId);
    expect(hit!.mode).toBe("observe");
  });

  it("parses a canonical succeeded list receipt with unknown status preserved", async () => {
    const listCommandId = LIST_COMMAND;
    const detail = await client.getWorkSourceCommand({ workspaceUuid: wsId, commandId: listCommandId });
    expect(detail.id).toBe(listCommandId);
    expect(detail.workspace_id).toBe(wsId);
    expect(detail.source_id).toBe(sourceId);
    expect(detail.command).toBe("list");
    expect(detail.request_id).toBe(LIST_REQUEST);
    expect(detail.limit_count).toBe(2);
    expect(detail.status).toBe("succeeded");

    const parsed = parseWorkSourceReadResult(detail);
    expect(parsed.command).toBe("list");
    if (parsed.command !== "list") throw new Error("unreachable");
    expect(parsed.items).toHaveLength(2);
    expect(parsed.items[0]).toEqual({
      nativeId: "bd-list-1",
      title: "Contract list one",
      status: "queued-weird",
      priority: 1,
      issueType: "task",
      description: undefined,
      dependencyCount: 0,
      dependentCount: 2,
      updatedAt: "",
    });
    expect(parsed.items[1]).toEqual({
      nativeId: "bd-list-2",
      title: "Contract list two",
      status: "open",
      priority: 0,
      issueType: "bug",
      description: "second",
      dependencyCount: 3,
      dependentCount: 0,
      updatedAt: "",
    });
  });

  it("parses a canonical succeeded detail receipt with opaque revision preserved", async () => {
    const detail = await client.getWorkSourceCommand({
      workspaceUuid: wsId,
      commandId: READ_COMMAND,
    });
    expect(detail.command).toBe("read");
    expect(detail.native_id).toBe("bd-detail-1");
    expect(detail.limit_count).toBeUndefined();
    expect(detail.status).toBe("succeeded");

    const parsed = parseWorkSourceReadResult(detail);
    expect(parsed.command).toBe("read");
    if (parsed.command !== "read") throw new Error("unreachable");
    expect(parsed.item).toEqual({
      nativeId: "bd-detail-1",
      title: "Contract detail",
      status: "in-progress",
      priority: 2,
      issueType: "story",
      description: undefined,
      revision: "rev opaque  trailing  ",
      dependencyCount: 1,
      dependentCount: 0,
      updatedAt: "",
    });
  });

  it("list responses strip results and keep workspace/source selectors", async () => {
    const rows = await client.listWorkSourceCommands({ workspaceUuid: wsId, sourceId });
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row.workspace_id).toBe(wsId);
      expect(row.source_id).toBe(sourceId);
      expect(row.result).toBeUndefined();
      expect(row.id).toMatch(/^[0-9a-f-]{36}$/i);
    }
  });

  it("identical create retry returns the terminal receipt unchanged", async () => {
    const receipt = await client.createWorkSourceCommand({
      workspaceUuid: wsId,
      sourceId,
      body: {
        request_id: LIST_REQUEST,
        command: "list",
        limit: 2,
      },
    });
    expect(receipt.id).toBe(LIST_COMMAND);
    expect(receipt.status).toBe("succeeded");
  });

  it("default list request decodes through the real query pipeline with wire limit_count undefined", async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const request = { request_id: DEFAULT_LIST_REQUEST, command: "list" as const };
    const options = workSourceReadOptions(wsId, sourceId, DEFAULT_LIST_COMMAND, request);
    // fetchQuery with the raw queryKey/queryFn returns the queryFn result
    // directly; options.select is exercised explicitly below.
    const receipt = await queryClient.fetchQuery({ queryKey: options.queryKey, queryFn: options.queryFn });
    expect(receipt.command).toBe("list");
    expect(receipt.request_id).toBe(DEFAULT_LIST_REQUEST);
    // The create body omitted `limit`, so the stored wire receipt must carry
    // no limit_count at all — the reproduced default-limit bug's contract.
    expect(receipt.limit_count).toBeUndefined();
    expect(receipt.status).toBe("succeeded");

    if (!options.select) throw new Error("workSourceReadOptions must define select");
    const decoded = options.select(receipt);
    expect(decoded.receipt.id).toBe(DEFAULT_LIST_COMMAND);
    if (decoded.result?.command !== "list") throw new Error("decoded result must be a list");
    expect(decoded.result.items).toHaveLength(2);
    expect(decoded.result.items[0]).toEqual({
      nativeId: "bd-default-1",
      title: "Default list one",
      status: "backlog-weird",
      priority: 0,
      issueType: "issue",
      description: undefined,
      dependencyCount: 0,
      dependentCount: 1,
      updatedAt: "",
    });
    expect(decoded.result.items[1]).toEqual({
      nativeId: "bd-default-2",
      title: "Default list two",
      status: "triaged-weird",
      priority: 1,
      issueType: "task",
      description: "default second",
      dependencyCount: 2,
      dependentCount: 0,
      updatedAt: "",
    });
  });
});
