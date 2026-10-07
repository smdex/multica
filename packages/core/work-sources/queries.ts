import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Workspace-scoped query keys; wsId is always part of the key. */
export const workSourceKeys = {
  all: (wsId: string) => ["work-sources", wsId] as const,
  list: (wsId: string) => [...workSourceKeys.all(wsId), "list"] as const,
  linksAll: (wsId: string) => [...workSourceKeys.all(wsId), "links"] as const,
  linksByIssue: (wsId: string, issueId: string) =>
    [...workSourceKeys.linksAll(wsId), "issue", issueId] as const,
  linksBySource: (wsId: string, sourceId: string) =>
    [...workSourceKeys.linksAll(wsId), "source", sourceId] as const,
};

export function workSourcesOptions(wsId: string) {
  return queryOptions({
    queryKey: workSourceKeys.list(wsId),
    queryFn: async () => {
      const sources = await api.listWorkSources({ workspaceUuid: wsId });
      if (sources.some((source) => source.workspace_id !== wsId)) throw new Error("Source list does not match the workspace");
      return sources;
    },
    enabled: !!wsId,
  });
}

export function issueWorkLinksByIssueOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: workSourceKeys.linksByIssue(wsId, issueId),
    queryFn: () => api.listIssueWorkLinks({ workspaceUuid: wsId, issueId }),
    enabled: !!wsId && !!issueId,
  });
}

export function issueWorkLinksBySourceOptions(wsId: string, sourceId: string) {
  return queryOptions({
    queryKey: workSourceKeys.linksBySource(wsId, sourceId),
    queryFn: () => api.listIssueWorkLinks({ workspaceUuid: wsId, sourceId }),
    enabled: !!wsId && !!sourceId,
  });
}

export const workSourceCommandKeys = {
  all: (wsId: string) => ["work-source-commands", wsId] as const,
  list: (wsId: string, sourceId: string) =>
    [...workSourceCommandKeys.all(wsId), "list", sourceId] as const,
  detail: (wsId: string, commandId: string) =>
    [...workSourceCommandKeys.all(wsId), "detail", commandId] as const,
};

export function workSourceCommandsOptions(wsId: string, sourceId: string) {
  return queryOptions({
    queryKey: workSourceCommandKeys.list(wsId, sourceId),
    queryFn: async ({ signal }) => {
      const receipts = await api.listWorkSourceCommands({ workspaceUuid: wsId, sourceId, signal });
      if (receipts.some((receipt) => receipt.workspace_id !== wsId || receipt.source_id !== sourceId)) throw new Error("Source history does not match the requested scope");
      return receipts;
    },
    enabled: !!wsId && !!sourceId,
  });
}

export function workSourceCommandOptions(wsId: string, commandId: string) {
  return queryOptions({
    queryKey: workSourceCommandKeys.detail(wsId, commandId),
    queryFn: ({ signal }) =>
      api.getWorkSourceCommand({ workspaceUuid: wsId, commandId, signal }),
    enabled: !!wsId && !!commandId,
  });
}
