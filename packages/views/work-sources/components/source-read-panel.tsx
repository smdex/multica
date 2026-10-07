"use client";

import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, RefreshCw } from "lucide-react";
import {
  workSourceReadOptions,
  useCreateWorkSourceRead,
} from "@multica/core/work-sources/reads";
import {
  workSourceCommandKeys,
  workSourceCommandsOptions,
} from "@multica/core/work-sources/queries";
import type {
  CreateWorkSourceCommandParams,
  WorkSource,
  WorkSourceCommand,
} from "@multica/core/types";
import type { WorkSourceReadResult } from "@multica/core/work-sources/read-result";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

const LIST_LIMIT = 50;

type Intent = CreateWorkSourceCommandParams;

/** Statuses that are not authoritative terminal outcomes. */
function uncertain(receipt: WorkSourceCommand | undefined): boolean {
  return (
    receipt?.status === "pending" ||
    receipt?.status === "claimed" ||
    receipt?.status === "unknown"
  );
}

function expired(receipt: WorkSourceCommand | undefined): boolean {
  return (
    !!receipt &&
    (receipt.status === "pending" || receipt.status === "claimed") &&
    Date.parse(receipt.expires_at) <= Date.now()
  );
}

/** One create mutation owns all explicit reads for this source. */
export function SourceReadPanel({
  wsId,
  source,
  canRequest,
}: {
  wsId: string;
  source: WorkSource;
  canRequest: boolean;
}) {
  const { t } = useT("sources");
  const qc = useQueryClient();
  const create = useCreateWorkSourceRead(wsId, source.id);

  // Local state holds only request intents and command pointers, never payloads.
  const [listIntent, setListIntent] = useState<Intent | null>(null);
  const [listCmdId, setListCmdId] = useState<string | null>(null);
  const [detailIntent, setDetailIntent] = useState<Intent | null>(null);
  const [detailCmdId, setDetailCmdId] = useState<string | null>(null);

  const historyQuery = useQuery(workSourceCommandsOptions(wsId, source.id));

  const listQuery = useQuery(
    workSourceReadOptions(wsId, source.id, listCmdId ?? "", listIntent ?? undefined),
  );
  const detailQuery = useQuery(
    workSourceReadOptions(wsId, source.id, detailCmdId ?? "", detailIntent ?? undefined),
  );

  // The source supports one command in flight. Stay fenced while a create
  // is pending, an unresolved create error still holds its mutation body
  // (same-UUID Retry only), or any tracked receipt (last accepted, kept even
  // if a later GET fails) is pending/claimed, including past its deadline,
  // until a manual recheck confirms a terminal status. Known history
  // pending/claimed receipts fence too.
  const unresolvedCreate = create.isPending || (create.isError && !!create.variables);
  // Prefer the accepted Query receipt; fall back to the cached detail entry
  // (same key) when a later GET failed, never to stale history metadata.
  const listReceipt = listQuery.data?.receipt ?? (listCmdId ? qc.getQueryData<WorkSourceCommand>(workSourceCommandKeys.detail(wsId, listCmdId)) : undefined);
  const detailReceipt = detailQuery.data?.receipt ?? (detailCmdId ? qc.getQueryData<WorkSourceCommand>(workSourceCommandKeys.detail(wsId, detailCmdId)) : undefined);
  // Creation stays fenced while history is loading or failed (we cannot know
  // whether a command is in flight) and while any known receipt, including
  // history rows, is pending/claimed/unknown.
  const historyUncertain =
    historyQuery.isPending ||
    historyQuery.isError ||
    (historyQuery.data ?? []).some((row) => {
      const latest = qc.getQueryData<WorkSourceCommand>(workSourceCommandKeys.detail(wsId, row.id));
      return uncertain(latest?.workspace_id === wsId && latest.source_id === source.id ? latest : row);
    });
  const busy =
    unresolvedCreate ||
    uncertain(listReceipt) ||
    uncertain(detailReceipt) ||
    historyUncertain;
  const canCreate = canRequest && source.enabled && !busy;

  const fire = (request: Intent, adopt: (id: string) => void) => {
    create.mutate(request, { onSuccess: (rc) => adopt(rc.id) });
  };

  const startList = () => {
    if (!canCreate) return;
    const request: Intent = { request_id: crypto.randomUUID(), command: "list", limit: LIST_LIMIT };
    setListIntent(request);
    setListCmdId(null);
    fire(request, setListCmdId);
  };

  // One click from a list row: point the detail intent at the item and fire.
  const startRead = (nativeId: string) => {
    if (!canCreate) return;
    const request: Intent = { request_id: crypto.randomUUID(), command: "read", native_id: nativeId };
    setDetailIntent(request);
    setDetailCmdId(null);
    fire(request, setDetailCmdId);
  };

  const listResult =
    listQuery.data?.result?.command === "list" ? listQuery.data.result : null;
  const detailResult =
    detailQuery.data?.result?.command === "read" ? detailQuery.data.result : null;

  const retryCreate = () => {
    // Transport failure retry reuses mutation.variables: the exact same body,
    // and a successful retry adopts the receipt into the owning pointer.
    const request = create.variables;
    if (!request || !create.isError) return;
    create.mutate(request, {
      onSuccess: (rc) => {
        if (request === listIntent) setListCmdId(rc.id);
        else if (request === detailIntent) setDetailCmdId(rc.id);
      },
    });
  };

  const recheck = (commandId: string | null) => {
    if (commandId) {
      void qc.refetchQueries({
        queryKey: workSourceCommandKeys.detail(wsId, commandId),
      });
    }
  };

  const commandStatus = (receipt: WorkSourceCommand | undefined, cmdId: string | null) => {
    if (receipt?.status === "failed") {
      return (
        <span key={cmdId} className="text-body text-destructive" role="alert">
          {t(($) => $.command_failed)}
          {receipt.error ? `: ${receipt.error}` : ""}
        </span>
      );
    }
    if (expired(receipt)) {
      return (
        <span key={cmdId} className="text-body text-muted-foreground" role="status">
          {t(($) => $.deadline_passed)}
          <Button
            type="button"
            variant="link"
            size="sm"
            className="px-1"
            onClick={() => recheck(cmdId)}
          >
            {t(($) => $.recheck)}
          </Button>
        </span>
      );
    }
    if (receipt?.status === "unknown") {
      // Unknown is never success or failure: stay busy, offer a manual recheck.
      return (
        <span key={cmdId} className="text-body text-muted-foreground" role="status">
          <Loader2 aria-hidden="true" className="mr-1 inline size-3 animate-spin" />
          {t(($) => $.status_unknown)}
          <Button
            type="button"
            variant="link"
            size="sm"
            className="px-1"
            onClick={() => recheck(cmdId)}
          >
            {t(($) => $.recheck)}
          </Button>
        </span>
      );
    }
    if (uncertain(receipt)) {
      const label =
        receipt?.status === "pending"
          ? t(($) => $.status_pending)
          : t(($) => $.status_claimed);
      return (
        <span key={cmdId} className="text-body text-muted-foreground" role="status">
          <Loader2 aria-hidden="true" className="mr-1 inline size-3 animate-spin" />
          {label}
        </span>
      );
    }
    return null;
  };

  return (
    <section className="flex flex-col gap-4" aria-label={t(($) => $.panel_aria)}>
      <p className="text-caption text-muted-foreground">
        {t(($) => $.read_only_caveat)}
      </p>
      {!source.enabled ? (
        <p className="text-body text-muted-foreground" role="status">
          {t(($) => $.source_disabled_hint)}
        </p>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={startList}
          disabled={!canCreate}
          aria-busy={busy}
        >
          {busy ? (
            <Loader2 aria-hidden="true" className="animate-spin" />
          ) : (
            <RefreshCw aria-hidden="true" />
          )}
          {t(($) => $.refresh)}
        </Button>
        {create.isError ? (
          <span className="text-body text-destructive" role="alert">
            {t(($) => $.create_error)}
            <Button type="button" variant="link" size="sm" className="px-1" onClick={retryCreate}>
              {t(($) => $.retry)}
            </Button>
          </span>
        ) : null}
        {commandStatus(listReceipt, listCmdId)}
        {commandStatus(detailReceipt, detailCmdId)}
        {historyQuery.isError ? (
          <span className="text-body text-destructive" role="alert">
            {t(($) => $.history_error)}
            <Button
              type="button"
              variant="link"
              size="sm"
              className="px-1"
              onClick={() => void qc.refetchQueries({ queryKey: workSourceCommandKeys.list(wsId, source.id) })}
            >
              {t(($) => $.recheck)}
            </Button>
          </span>
        ) : null}
        {(listQuery.isError || detailQuery.isError) && !create.isError ? (
          <span className="text-body text-destructive" role="alert">
            {t(($) => $.receipt_invalid)}
            {listQuery.isError && listCmdId ? (
              <Button
                type="button"
                variant="link"
                size="sm"
                className="px-1"
                onClick={() => recheck(listCmdId)}
              >
                {t(($) => $.recheck)}
              </Button>
            ) : null}
            {detailQuery.isError && detailCmdId ? (
              <Button
                type="button"
                variant="link"
                size="sm"
                className="px-1"
                onClick={() => recheck(detailCmdId)}
              >
                {t(($) => $.recheck)}
              </Button>
            ) : null}
          </span>
        ) : null}
      </div>

      {listResult ? (
        <>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.first_n_hint, { count: LIST_LIMIT })}
          </p>
          <ul className="divide-y rounded-md border">
            {listResult.items.map((item) => (
              <li key={item.nativeId} className="flex min-w-0 items-center gap-2 p-2">
                <div className="min-w-0 flex-1">
                  <p className="text-body truncate font-medium">{item.title}</p>
                  <p className="text-caption text-muted-foreground truncate">
                    {item.nativeId} · {item.status}
                  </p>
                </div>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="shrink-0"
                  onClick={() => startRead(item.nativeId)}
                  disabled={!canCreate}
                >
                  {t(($) => $.item_read)}
                </Button>
              </li>
            ))}
          </ul>
        </>
      ) : null}

      {detailResult ? (
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 rounded-md border p-3">
          <dt className="text-label text-muted-foreground">{t(($) => $.field_title)}</dt>
          <dd className="text-body">{detailResult.item.title}</dd>
          <dt className="text-label text-muted-foreground">{t(($) => $.field_status)}</dt>
          <dd className="text-body">{detailResult.item.status}</dd>
          <dt className="text-label text-muted-foreground">{t(($) => $.field_revision)}</dt>
          <dd className="text-body">{detailResult.item.revision}</dd>
          {detailResult.item.description ? (
            <>
              <dt className="text-label text-muted-foreground">
                {t(($) => $.field_description)}
              </dt>
              <dd className="text-body whitespace-pre-wrap">
                {detailResult.item.description}
              </dd>
            </>
          ) : null}
        </dl>
      ) : null}
    </section>
  );
}

/** Rebuild the create request a history receipt must answer, from its metadata. */
function requestFromReceipt(
  receipt: WorkSourceCommand,
): CreateWorkSourceCommandParams {
  return receipt.command === "read"
    ? {
        request_id: receipt.request_id,
        command: "read",
        native_id: receipt.native_id,
      }
    : {
        request_id: receipt.request_id,
        command: "list",
        limit: receipt.limit_count,
      };
}

/**
 * Everyone, including ordinary members, can inspect existing command history:
 * select a receipt, refetch it through the scoped read query, and decode its
 * result. No new command is ever created here.
 */
export function SourceHistoryPanel({
  wsId,
  source,
}: {
  wsId: string;
  source: WorkSource;
}) {
  const { t } = useT("sources");
  const qc = useQueryClient();
  const history = useQuery(workSourceCommandsOptions(wsId, source.id));
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = history.data?.find((c) => c.id === selectedId) ?? null;
  const selectedQuery = useQuery(
    workSourceReadOptions(
      wsId,
      source.id,
      selected?.id ?? "",
      selected ? requestFromReceipt(selected) : undefined,
    ),
  );
  const result: WorkSourceReadResult | undefined = selectedQuery.data?.result;
  const status = selectedQuery.data?.receipt.status;
  useEffect(() => {
    if (status === "succeeded" || status === "failed") {
      void qc.invalidateQueries({ queryKey: workSourceCommandKeys.list(wsId, source.id) });
    }
  }, [qc, source.id, wsId, selectedId, status]);

  return (
    <section className="flex flex-col gap-2" aria-label={t(($) => $.history_aria)}>
      <label
        htmlFor="work-source-history"
        className="text-label shrink-0 font-medium"
      >
        {t(($) => $.history_label)}
      </label>
      <select
        id="work-source-history"
        className="text-body h-9 max-w-full min-w-0 rounded-md border bg-background px-2 focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring"
        value={selectedId ?? ""}
        onChange={(event) => setSelectedId(event.target.value || null)}
      >
        <option value="">{t(($) => $.history_none)}</option>
        {(history.data ?? []).map((c) => (
          <option key={c.id} value={c.id}>
            {c.command}
            {c.native_id ? ` ${c.native_id}` : ""}
            {c.status === "succeeded" ? "" : ` · ${c.status}`}
          </option>
        ))}
      </select>
      {selectedQuery.isError || uncertain(selectedQuery.data?.receipt ?? selected ?? undefined) ? (
        <p className="text-body text-muted-foreground" role="status">
          {selectedQuery.isError ? t(($) => $.receipt_invalid) : t(($) => $.status_unknown)}
          <Button type="button" variant="link" size="sm" onClick={() => void selectedQuery.refetch()}>
            {t(($) => $.recheck)}
          </Button>
        </p>
      ) : null}
      {result?.command === "list" ? (
        <ul className="divide-y rounded-md border">
          {result.items.map((item) => (
            <li key={item.nativeId} className="p-2">
              <p className="text-body truncate font-medium">{item.title}</p>
              <p className="text-caption text-muted-foreground truncate">
                {item.nativeId} · {item.status}
              </p>
            </li>
          ))}
        </ul>
      ) : null}
      {result?.command === "read" ? (
        <p className="text-caption text-muted-foreground">
          {result.item.title} · {result.item.revision}
        </p>
      ) : null}
    </section>
  );
}
