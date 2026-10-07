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
    queryFn: () => api.listWorkSources({ workspaceUuid: wsId }),
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
