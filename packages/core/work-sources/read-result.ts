import { z } from "zod";
import { parseWithFallback } from "../api/schema";
import type { WorkSourceCommand } from "../types";

const utf8 = new TextEncoder();

function utf8ByteLength(s: string): number {
  return utf8.encode(s).length;
}

/** Cap on receipt.result size; matches the server's 2 MiB source-result bound. */
export const MAX_READ_RESULT_BYTES = 2 * 1024 * 1024;

export interface SourceWorkItem {
  nativeId: string;
  title: string;
  status: string;
  priority: number;
  issueType: string;
  description?: string;
  revision?: string;
  dependencyCount: number;
  dependentCount: number;
  updatedAt?: string;
}

export type WorkSourceReadResult =
  | { command: "list"; items: SourceWorkItem[] }
  | { command: "read"; item: SourceWorkItem & { revision: string } };

const ENDPOINT = "work-source read-result";

const countSchema = z
  .number()
  .int()
  .finite()
  .nonnegative()
  .optional()
  .default(0);

/** Canonical Go IssueSummary wire shape (snake_case). Unknown fields are discarded. */
const issueSummarySchema = z.object({
  id: z.string(),
  title: z.string(),
  status: z.string(),
  priority: z.number().int().finite(),
  issue_type: z.string(),
  description: z.string().nullable().optional(),
  updated_at: z.string().nullable().optional(),
  dependency_count: countSchema,
  dependent_count: countSchema,
});

const issueSchema = issueSummarySchema.extend({
  revision: z.string(),
});

const summaryListSchema = z.array(issueSummarySchema);

function mapSummary(raw: z.infer<typeof issueSummarySchema>): SourceWorkItem {
  return {
    nativeId: raw.id,
    title: raw.title,
    status: raw.status,
    priority: raw.priority,
    issueType: raw.issue_type,
    description: raw.description ?? undefined,
    dependencyCount: raw.dependency_count,
    dependentCount: raw.dependent_count,
    updatedAt: raw.updated_at ?? undefined,
  };
}

function decodeListPayload(raw: unknown): z.infer<typeof summaryListSchema> {
  // A list payload is a bare array of summaries; an object (or anything else)
  // fails closed here before parseWithFallback logging.
  const parsed = parseWithFallback<z.infer<typeof summaryListSchema> | null>(
    raw,
    summaryListSchema,
    null,
    { endpoint: ENDPOINT },
  );
  if (parsed === null) {
    throw new Error("work-source list result failed schema validation");
  }
  return parsed;
}

function decodeReadPayload(raw: unknown): z.infer<typeof issueSchema> {
  // The receipt contains the canonical Issue object, not bd show's CLI array.
  const item = parseWithFallback<z.infer<typeof issueSchema> | null>(
    raw,
    issueSchema,
    null,
    { endpoint: ENDPOINT },
  );
  if (item === null) {
    throw new Error("work-source read result failed schema validation");
  }
  return item;
}

/**
 * Decode the `result` JSON of a succeeded work-source command receipt into
 * camelCase work items. Throws on anything that is not a clean success:
 * non-succeeded status, missing/oversized result, invalid JSON, wire-shape
 * drift, command/param mismatch, duplicate or blank native IDs, list results
 * over the requested limit, a read answering a different ID, or a blank
 * revision. Never invents dependency edges or defaults beyond count zeros.
 */
export function parseWorkSourceReadResult(
  receipt: WorkSourceCommand,
): WorkSourceReadResult {
  if (receipt.status !== "succeeded") {
    throw new Error(
      `work-source command ${receipt.id} is ${receipt.status}, not succeeded`,
    );
  }
  const result = receipt.result;
  if (typeof result !== "string" || result.length === 0) {
    throw new Error(
      `work-source command ${receipt.id} has no result to decode`,
    );
  }
  if (utf8ByteLength(result) > MAX_READ_RESULT_BYTES) {
    throw new Error(
      `work-source command ${receipt.id} result exceeds 2 MiB`,
    );
  }

  let raw: unknown;
  try {
    raw = JSON.parse(result);
  } catch {
    throw new Error(
      `work-source command ${receipt.id} result is not valid JSON`,
    );
  }

  if (receipt.command === "list") {
    const limit = receipt.limit_count ?? 50;
    if (typeof limit !== "number" || !Number.isInteger(limit) || limit < 1 || limit > 200) {
      throw new Error(
        `work-source list command ${receipt.id} has invalid limit_count ${String(limit)}`,
      );
    }
    const rows = decodeListPayload(raw);
    if (rows.length > limit) {
      throw new Error(
        `work-source list command ${receipt.id} returned ${rows.length} items, over limit ${limit}`,
      );
    }
    const seen = new Set<string>();
    for (const row of rows) {
      if (row.id.trim() === "") {
        throw new Error(
          `work-source list command ${receipt.id} returned a blank native id`,
        );
      }
      if (seen.has(row.id)) {
        throw new Error(
          `work-source list command ${receipt.id} returned duplicate native id ${row.id}`,
        );
      }
      seen.add(row.id);
    }
    return { command: "list", items: rows.map(mapSummary) };
  }

  // command === "read"
  const nativeId = receipt.native_id;
  if (typeof nativeId !== "string" || nativeId.trim() === "") {
    throw new Error(
      `work-source read command ${receipt.id} has no native_id`,
    );
  }
  const row = decodeReadPayload(raw);
  if (row.id !== nativeId) {
    throw new Error(
      `work-source read command ${receipt.id} asked for ${nativeId} but got ${row.id}`,
    );
  }
  if (row.revision.trim() === "") {
    throw new Error(
      `work-source read command ${receipt.id} returned a blank revision`,
    );
  }
  return {
    command: "read",
    item: { ...mapSummary(row), revision: row.revision },
  };
}
