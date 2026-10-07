import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { CreateWorkSourceCommandParams, WorkSourceCommand } from "../types/work-source";
import { parseWorkSourceReadResult } from "./read-result";
import { workSourceCommandKeys } from "./queries";

/** A receipt must answer this exact user intent, never another source or request. */
export function assertWorkSourceReadReceipt(
  receipt: WorkSourceCommand,
  wsId: string,
  sourceId: string,
  request: CreateWorkSourceCommandParams,
  commandId?: string,
): WorkSourceCommand {
  if (
    receipt.workspace_id !== wsId ||
    receipt.source_id !== sourceId ||
    receipt.request_id !== request.request_id ||
    receipt.command !== request.command ||
    (commandId !== undefined && receipt.id !== commandId) ||
    (request.command === "read" &&
      (receipt.native_id !== request.native_id || receipt.limit_count !== undefined)) ||
    (request.command === "list" &&
      (receipt.limit_count !== request.limit || receipt.native_id !== undefined))
  ) {
    throw new Error("Source read receipt does not match the request");
  }
  return receipt;
}

/** A passed deadline is not proof of server failure. Stop automatic polling, allow manual recheck. */
export function sourceReadPollInterval(receipt: WorkSourceCommand | undefined, now = Date.now()) {
  if (!receipt || (receipt.status !== "pending" && receipt.status !== "claimed" && receipt.status !== "unknown")) return false;
  return Date.parse(receipt.expires_at) > now ? 2000 : false;
}

export function workSourceReadOptions(
  wsId: string,
  sourceId: string,
  commandId: string,
  request: CreateWorkSourceCommandParams | undefined,
) {
  return queryOptions({
    queryKey: workSourceCommandKeys.detail(wsId, commandId),
    queryFn: async ({ signal }) => {
      if (!request) throw new Error("Source read request is missing");
      return assertWorkSourceReadReceipt(
        await api.getWorkSourceCommand({ workspaceUuid: wsId, commandId, signal }),
        wsId, sourceId, request, commandId,
      );
    },
    enabled: !!wsId && !!sourceId && !!commandId && !!request,
    retry: false,
    refetchInterval: (query) => query.state.error ? false : sourceReadPollInterval(query.state.data),
    select: (receipt: WorkSourceCommand) => {
      if (!request) throw new Error("Source read request is missing");
      assertWorkSourceReadReceipt(receipt, wsId, sourceId, request, commandId);
      return {
        receipt,
        result: receipt.status === "succeeded" ? parseWorkSourceReadResult(receipt) : undefined,
      };
    },
  });
}

/** The caller owns one UUID per explicit intent and reuses the same body after transport loss. */
export function useCreateWorkSourceRead(wsId: string, sourceId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationKey: [...workSourceCommandKeys.all(wsId), "create", sourceId],
    retry: false,
    mutationFn: async (request: CreateWorkSourceCommandParams) => {
      const receipt = await api.createWorkSourceCommand({ workspaceUuid: wsId, sourceId, body: request });
      return assertWorkSourceReadReceipt(receipt, wsId, sourceId, request);
    },
    onSuccess: (receipt) => {
      qc.setQueryData(workSourceCommandKeys.detail(wsId, receipt.id), receipt);
      void qc.invalidateQueries({ queryKey: workSourceCommandKeys.list(wsId, sourceId) });
    },
  });
}
