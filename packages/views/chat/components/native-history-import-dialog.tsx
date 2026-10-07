"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@multica/ui/components/ui/dialog";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useImportNativeSession, useListNativeSessions } from "@multica/core/chat/mutations";
import { chatSessionsOptions, workflowCapabilitiesOptions, workflowRequestOptions } from "@multica/core/chat/queries";
import { runtimeDisplayLabel, runtimeListOptions } from "@multica/core/runtimes";
import type {
  Agent,
  NativeSessionListResult,
  NativeSessionSummary,
  WorkflowRequest,
} from "@multica/core/types";
import { useT } from "../../i18n";

interface ListRequest {
  id: string;
  runtimeId: string;
  cursor: string | null;
}

interface ImportRequest {
  id: string;
  runtimeId: string;
}

function listResult(request: WorkflowRequest | undefined): NativeSessionListResult | null {
  if (
    request?.kind !== "native_session_list" ||
    request.status !== "completed" ||
    !request.result ||
    !("sessions" in request.result)
  ) {
    return null;
  }
  return request.result;
}

function importChatSessionId(request: WorkflowRequest | undefined): string | null {
  if (
    request?.kind !== "native_session_import" ||
    request.status !== "completed" ||
    !request.result ||
    !("chat_session_id" in request.result) ||
    !request.result.chat_session_id
  ) {
    return null;
  }
  return request.result.chat_session_id;
}

/**
 * Browses server-redacted native history and imports only an explicit selected
 * source into an explicit destination persona. The only local list state is
 * request identity and pagination intent; workflow response pages stay in the
 * Query cache, which prevents a second server-data store from drifting.
 */
export function NativeHistoryImportDialog({
  wsId,
  agents,
  onImported,
}: {
  wsId: string;
  agents: Agent[];
  onImported: (chatSessionId: string) => void;
}) {
  const { t } = useT("chat");
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [runtimeId, setRuntimeId] = useState<string | null>(null);
  const [selectedSession, setSelectedSession] = useState<NativeSessionSummary | null>(null);
  const [destinationAgentId, setDestinationAgentId] = useState<string | null>(null);
  const [listPages, setListPages] = useState<ListRequest[]>([]);
  const [importRequest, setImportRequest] = useState<ImportRequest | null>(null);
  const [listFailed, setListFailed] = useState(false);
  const [importFailed, setImportFailed] = useState(false);
  const [refreshingImportedChat, setRefreshingImportedChat] = useState(false);
  const selectedRuntimeRef = useRef<string | null>(null);
  const listInFlightRef = useRef(false);
  const importInFlightRef = useRef(false);
  const importedRequestRef = useRef<string | null>(null);
  const onImportedRef = useRef(onImported);

  useEffect(() => {
    onImportedRef.current = onImported;
  }, [onImported]);

  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId, "me"));
  const { data: capabilities } = useQuery(
    workflowCapabilitiesOptions(wsId, runtimeId ?? ""),
  );
  const { mutateAsync: listNativeSessions, isPending: isListMutationPending } = useListNativeSessions();
  const { mutateAsync: importNativeSession, isPending: isImportMutationPending } = useImportNativeSession();
  const pageQueries = useQueries({
    queries: listPages.map((page) => workflowRequestOptions(wsId, page.runtimeId, page.id)),
  });
  const { data: importOperation } = useQuery(
    workflowRequestOptions(wsId, importRequest?.runtimeId ?? "", importRequest?.id ?? ""),
  );
  const destinationAgents = useMemo(
    () => agents.filter((agent) => agent.runtime_id === runtimeId),
    [agents, runtimeId],
  );
  const canBrowse = Boolean(capabilities?.online && capabilities.native_sessions.list);
  const canImport = Boolean(capabilities?.online && capabilities.native_sessions.import);
  const currentPages = listPages
    .map((page, index) => ({ page, operation: pageQueries[index]?.data }))
    .filter(({ page }) => page.runtimeId === runtimeId);
  const settledListPages = currentPages.flatMap(({ page, operation }) => {
    const result = listResult(operation);
    return result ? [{ page, result }] : [];
  });
  const nativeSessions = useMemo(() => {
    const byRef = new Map<string, NativeSessionSummary>();
    for (const { result } of settledListPages) {
      for (const session of result.sessions) byRef.set(session.session_ref, session);
    }
    return [...byRef.values()];
  }, [settledListPages]);
  const latestListPage = settledListPages.at(-1)?.result ?? null;
  const nextCursor = latestListPage?.next_cursor ?? null;
  const truncated = settledListPages.some(({ result }) => result.truncated);
  const listPending = isListMutationPending || currentPages.some(
    ({ operation }) => operation?.status === "pending" || operation?.status === "running",
  );
  const listUnknown = currentPages.some(({ operation }) => operation?.status === "unknown");
  const listOperationFailed = currentPages.some(({ operation }) => operation?.status === "failed");
  const importChatId = importChatSessionId(importOperation);
  const importPending = isImportMutationPending ||
    importOperation?.status === "pending" ||
    importOperation?.status === "running";
  const importUnknown = importOperation?.status === "unknown";
  const importBlocked = importPending || importUnknown || refreshingImportedChat || importInFlightRef.current;

  useEffect(() => {
    selectedRuntimeRef.current = runtimeId;
  }, [runtimeId]);

  const startList = useCallback((targetRuntimeId: string, cursor: string | null) => {
    if (listInFlightRef.current) return;
    listInFlightRef.current = true;
    setListFailed(false);
    void listNativeSessions({ runtimeId: targetRuntimeId, cursor }).then(
      (request) => {
        if (!request.id || selectedRuntimeRef.current !== targetRuntimeId) return;
        setListPages((current) => current.some((page) => page.id === request.id)
          ? current
          : [...current, { id: request.id, runtimeId: targetRuntimeId, cursor }]);
      },
      () => {
        if (selectedRuntimeRef.current === targetRuntimeId) setListFailed(true);
      },
    ).finally(() => {
      listInFlightRef.current = false;
    });
  }, [listNativeSessions]);

  useEffect(() => {
    if (!open || !runtimeId || !canBrowse || listPages.some((page) => page.runtimeId === runtimeId)) {
      return;
    }
    startList(runtimeId, null);
  }, [canBrowse, listPages, open, runtimeId, startList]);

  useEffect(() => {
    if (!importChatId || !importRequest || importedRequestRef.current === importRequest.id) return;
    let cancelled = false;
    setRefreshingImportedChat(true);
    setImportFailed(false);

    // A completed import commits a chat transactionally, but this surface's
    // session list may still be Infinity-fresh. Populate the authoritative
    // Query cache before selecting the ID so the controller never clears an
    // otherwise valid, just-imported session as unknown.
    void queryClient.fetchQuery({
      ...chatSessionsOptions(wsId),
      staleTime: 0,
    }).then(
      (sessions) => {
        if (cancelled) return;
        importedRequestRef.current = importRequest.id;
        setRefreshingImportedChat(false);
        if (!sessions.some((session) => session.id === importChatId)) {
          setImportFailed(true);
          return;
        }
        setOpen(false);
        onImportedRef.current(importChatId);
      },
      () => {
        if (cancelled) return;
        importedRequestRef.current = importRequest.id;
        setRefreshingImportedChat(false);
        setImportFailed(true);
      },
    );

    return () => {
      cancelled = true;
    };
  }, [importChatId, importRequest, queryClient, wsId]);

  const changeRuntime = (nextRuntimeId: string | null) => {
    if (importBlocked) return;
    selectedRuntimeRef.current = nextRuntimeId;
    setRuntimeId(nextRuntimeId);
    setSelectedSession(null);
    setDestinationAgentId(null);
    setListPages([]);
    setImportRequest(null);
    setListFailed(false);
    setImportFailed(false);
    setRefreshingImportedChat(false);
    importedRequestRef.current = null;
  };

  const loadMore = () => {
    if (!runtimeId || !nextCursor || listPending || listUnknown) return;
    startList(runtimeId, nextCursor);
  };

  const startImport = () => {
    if (
      !runtimeId ||
      !selectedSession ||
      !destinationAgentId ||
      !canImport ||
      importBlocked
    ) {
      return;
    }
    importInFlightRef.current = true;
    setImportFailed(false);
    void importNativeSession({
      runtimeId,
      sessionRef: selectedSession.session_ref,
      revision: selectedSession.revision,
      agentId: destinationAgentId,
    }).then(
      (request) => {
        if (!request.id || selectedRuntimeRef.current !== runtimeId) {
          setImportFailed(true);
          return;
        }
        setImportRequest({ id: request.id, runtimeId });
      },
      () => setImportFailed(true),
    ).finally(() => {
      importInFlightRef.current = false;
    });
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(nextOpen) => {
        if (!nextOpen && importBlocked) return;
        setOpen(nextOpen);
      }}
    >
      <DialogTrigger render={<Button variant="outline" size="sm" data-slot="native-history-import-trigger" />}>
        {t(($) => $.workflow.import_history)}
      </DialogTrigger>
      <DialogContent data-slot="native-history-import-dialog" className="sm:max-w-lg" showCloseButton={!importBlocked}>
        <DialogHeader>
          <DialogTitle>{t(($) => $.workflow.import_title)}</DialogTitle>
          <DialogDescription>{t(($) => $.workflow.import_description)}</DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="native-history-runtime">{t(($) => $.workflow.runtime)}</Label>
            <Select
              items={runtimes.map((runtime) => ({ value: runtime.id, label: runtimeDisplayLabel(runtime) }))}
              value={runtimeId}
              onValueChange={changeRuntime}
              disabled={importBlocked}
            >
              <SelectTrigger id="native-history-runtime" data-slot="native-history-runtime" className="w-full">
                <SelectValue placeholder={t(($) => $.workflow.select_runtime)} />
              </SelectTrigger>
              <SelectContent>
                {runtimes.map((runtime) => (
                  <SelectItem key={runtime.id} value={runtime.id}>
                    {runtimeDisplayLabel(runtime)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {runtimeId && capabilities && !canBrowse && (
            <p role="status" className="text-caption text-muted-foreground">{capabilities.reason ?? t(($) => $.workflow.history_empty)}</p>
          )}
          {capabilities?.native_sessions.list && !capabilities.native_sessions.import && (
            <p role="status" className="text-caption text-muted-foreground">{t(($) => $.workflow.browse_only)}</p>
          )}
          {listPending ? (
            <p role="status" className="flex items-center gap-1.5 text-caption text-muted-foreground">
              <Loader2 className="size-3 animate-spin" />
              {t(($) => $.workflow.loading_history)}
            </p>
          ) : null}
          {listFailed || listOperationFailed ? (
            <p role="alert" className="text-caption text-destructive">{t(($) => $.workflow.request_failed)}</p>
          ) : null}
          {listUnknown && (
            <p role="status" className="text-caption text-muted-foreground">{t(($) => $.workflow.operation_unknown)}</p>
          )}

          {nativeSessions.length > 0 && (
            <fieldset className="space-y-2">
              <legend className="text-label font-medium">{t(($) => $.workflow.select_history)}</legend>
              <div className="max-h-52 space-y-1 overflow-y-auto rounded-md border border-border p-1">
                {nativeSessions.map((session) => (
                  <label key={session.session_ref} className="flex cursor-pointer items-start gap-2 rounded-md px-2 py-1.5 hover:bg-muted">
                    <input
                      data-slot="native-history-session"
                      type="radio"
                      name="native-history-session"
                      checked={selectedSession?.session_ref === session.session_ref}
                      onChange={() => setSelectedSession(session)}
                    />
                    <span className="min-w-0">
                      <span className="block truncate text-body">{session.title}</span>
                      {session.preview && <span className="block truncate text-caption text-muted-foreground">{session.preview}</span>}
                      {session.cwd && <span className="block truncate text-caption text-muted-foreground">{t(($) => $.workflow.working_directory, { cwd: session.cwd })}</span>}
                    </span>
                  </label>
                ))}
              </div>
              {nextCursor && (
                <Button type="button" size="xs" variant="outline" onClick={loadMore} disabled={listPending || listUnknown}>
                  {t(($) => $.workflow.load_more)}
                </Button>
              )}
              {truncated && <p className="text-caption text-muted-foreground">{t(($) => $.workflow.history_truncated)}</p>}
            </fieldset>
          )}
          {runtimeId && canBrowse && !listPending && settledListPages.length > 0 && nativeSessions.length === 0 && (
            <p className="text-caption text-muted-foreground">{t(($) => $.workflow.history_empty)}</p>
          )}

          <div className="space-y-1.5">
            <Label htmlFor="native-history-destination">{t(($) => $.workflow.destination_agent)}</Label>
            <Select
              items={destinationAgents.map((agent) => ({ value: agent.id, label: agent.name }))}
              value={destinationAgentId}
              onValueChange={setDestinationAgentId}
              disabled={importBlocked}
            >
              <SelectTrigger id="native-history-destination" data-slot="native-history-destination" className="w-full">
                <SelectValue placeholder={t(($) => $.workflow.select_agent)} />
              </SelectTrigger>
              <SelectContent>
                {destinationAgents.map((agent) => (
                  <SelectItem key={agent.id} value={agent.id}>{agent.name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {selectedSession && destinationAgentId && (
            <p className="text-caption text-muted-foreground">{t(($) => $.workflow.copy_notice)}</p>
          )}
          {importPending || refreshingImportedChat ? (
            <p role="status" className="text-caption text-muted-foreground">{t(($) => $.workflow.operation_pending)}</p>
          ) : null}
          {importUnknown && (
            <p role="status" className="text-caption text-muted-foreground">{t(($) => $.workflow.operation_unknown)}</p>
          )}
          {importFailed || importOperation?.status === "failed" ? (
            <p role="alert" className="text-caption text-destructive">{importOperation?.error?.message || t(($) => $.workflow.operation_failed)}</p>
          ) : null}
          {importOperation?.result && "warnings" in importOperation.result && importOperation.result.warnings.length > 0 && (
            <div className="space-y-1 text-caption text-muted-foreground">
              <p className="font-medium text-foreground">{t(($) => $.workflow.import_warnings)}</p>
              {importOperation.result.warnings.map((warning) => <p key={warning}>{warning}</p>)}
            </div>
          )}
        </div>

        <DialogFooter>
          <DialogClose render={<Button type="button" variant="outline" disabled={importBlocked} />}>
            {t(($) => $.workflow.cancel)}
          </DialogClose>
          <Button
            type="button"
            data-slot="native-history-import-submit"
            onClick={startImport}
            disabled={!canImport || !selectedSession || !destinationAgentId || importBlocked}
            aria-busy={importPending || undefined}
          >
            {importPending ? t(($) => $.workflow.importing) : t(($) => $.workflow.import_action)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
