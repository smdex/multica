// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { WorkSourceCommand } from "../types";
import { parseWorkSourceReadResult } from "./read-result";

function receipt(overrides: Partial<WorkSourceCommand>): WorkSourceCommand {
  return {
    id: "cmd-1",
    request_id: "req-1",
    workspace_id: "ws-1",
    source_id: "src-1",
    command: "list",
    status: "succeeded",
    config_revision: 1,
    expires_at: "2026-01-01T00:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

const summary = {
  id: "ISSUE-1",
  title: "Fix flaky build",
  status: "open",
  priority: 3,
  issue_type: "bug",
  description: "nightly job fails",
  updated_at: "2026-02-03T04:05:06Z",
  dependency_count: 2,
  dependent_count: 1,
};

const listReceipt = (result: string, limit_count = 50) =>
  receipt({ command: "list", limit_count, result });

const readReceipt = (result: string, native_id = "ISSUE-1") =>
  receipt({ command: "read", native_id, result });

describe("parseWorkSourceReadResult", () => {
  it("decodes a valid list with camelCase mapping and count defaults", () => {
    const minimal = {
      id: "ISSUE-2",
      title: "t",
      status: "weird-status",
      priority: 0,
      issue_type: "task",
    };
    const out = parseWorkSourceReadResult(
      listReceipt(JSON.stringify([summary, minimal])),
    );
    expect(out).toEqual({
      command: "list",
      items: [
        {
          nativeId: "ISSUE-1",
          title: "Fix flaky build",
          status: "open",
          priority: 3,
          issueType: "bug",
          description: "nightly job fails",
          dependencyCount: 2,
          dependentCount: 1,
          updatedAt: "2026-02-03T04:05:06Z",
        },
        {
          nativeId: "ISSUE-2",
          title: "t",
          status: "weird-status",
          priority: 0,
          issueType: "task",
          description: undefined,
          dependencyCount: 0,
          dependentCount: 0,
          updatedAt: undefined,
        },
      ],
    });
  });

  it("decodes a valid read with required revision", () => {
    const out = parseWorkSourceReadResult(
      readReceipt(JSON.stringify({ ...summary, revision: "abc123" })),
    );
    expect(out).toEqual({
      command: "read",
      item: {
        nativeId: "ISSUE-1",
        title: "Fix flaky build",
        status: "open",
        priority: 3,
        issueType: "bug",
        description: "nightly job fails",
        dependencyCount: 2,
        dependentCount: 1,
        updatedAt: "2026-02-03T04:05:06Z",
        revision: "abc123",
      },
    });
  });

  it("preserves opaque whitespace in native IDs and revision", () => {
    const id = "  x y  ";
    const out = parseWorkSourceReadResult(
      readReceipt(JSON.stringify({ ...summary, id, revision: " r " }), id),
    ) as Extract<
      ReturnType<typeof parseWorkSourceReadResult>,
      { command: "read" }
    >;
    expect(out.item.nativeId).toBe(id);
    expect(out.item.revision).toBe(" r ");
  });

  it("accepts the server maximum and bounds UTF-8 bytes, not character count", () => {
    expect(parseWorkSourceReadResult(listReceipt("[]", 200))).toEqual({ command: "list", items: [] });
    const result = JSON.stringify([{ ...summary, description: "界".repeat(800_000) }]);
    expect(result.length).toBeLessThan(2 * 1024 * 1024);
    expect(() => parseWorkSourceReadResult(listReceipt(result))).toThrow("exceeds 2 MiB");
  });

  it("uses the server default bound when the receipt omits limit_count", () => {
    expect(parseWorkSourceReadResult(receipt({ result: "[]" }))).toEqual({ command: "list", items: [] });
    const rows = Array.from({ length: 51 }, (_, i) => ({ ...summary, id: `item-${i}` }));
    expect(() => parseWorkSourceReadResult(receipt({ result: JSON.stringify(rows) }))).toThrow();
  });

  const rejects: Array<[string, WorkSourceCommand]> = [
    ["pending status", { ...listReceipt("[]"), status: "pending" }],
    ["claimed status", { ...listReceipt("[]"), status: "claimed" }],
    ["failed status", { ...listReceipt("[]"), status: "failed" }],
    ["missing result", listReceipt(undefined as unknown as string)],
    ["empty result", listReceipt("")],
    [
      "result over 2 MiB",
      listReceipt('["' + "a".repeat(2 * 1024 * 1024 + 10) + '"]'),
    ],
    ["invalid JSON", listReceipt("{not json")],
    ["JSON null", listReceipt("null")],
    [
      "list payload is an object",
      listReceipt(JSON.stringify({ items: [summary] })),
    ],
    [
      "read payload is a CLI array",
      readReceipt(JSON.stringify([{ ...summary, revision: "r" }])),
    ],
    [
      "missing required field",
      listReceipt(
        JSON.stringify([{ ...summary, title: undefined }].map((s) => {
          const { title, ...rest } = s;
          void title;
          return rest;
        })),
      ),
    ],
    ["non-integer priority", listReceipt(JSON.stringify([{ ...summary, priority: 1.5 }]))],
    ["negative dependency count", listReceipt(JSON.stringify([{ ...summary, dependency_count: -1 }]))],
    ["string count", listReceipt(JSON.stringify([{ ...summary, dependency_count: "2" }]))],

    ["limit_count zero", listReceipt("[]", 0)],
    ["limit_count over 200", listReceipt("[]", 201)],
    ["non-integer limit_count", listReceipt("[]", 10.5)],
    ["list over limit", listReceipt(JSON.stringify([summary, { ...summary, id: "B" }]), 1)],
    ["duplicate native ids", listReceipt(JSON.stringify([summary, summary]))],
    ["trim-blank native id", listReceipt(JSON.stringify([{ ...summary, id: "   " }]))],
    ["read missing native_id", { ...readReceipt(JSON.stringify({ ...summary, revision: "r" })) , native_id: undefined }],
    [
      "read answering different id",
      readReceipt(JSON.stringify({ ...summary, revision: "r" }), "OTHER"),
    ],
    [
      "read with blank revision",
      readReceipt(JSON.stringify({ ...summary, revision: "  " })),
    ],
    [
      "read with missing revision",
      readReceipt(JSON.stringify(summary)),
    ],
    [
      "read returning two rows",
      readReceipt(
        JSON.stringify([
          { ...summary, revision: "r" },
          { ...summary, id: "B", revision: "r" },
        ]),
      ),
    ],
  ];

  it.each(rejects)("rejects %s", (_name, bad) => {
    expect(() => parseWorkSourceReadResult(bad)).toThrow();
  });
});
