"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Database, TriangleAlert } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { workSourcesOptions } from "@multica/core/work-sources/queries";
import type { WorkSource } from "@multica/core/types";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  CollectionPageHeader,
  CollectionPageState,
} from "../../layout/collection-page";
import { useT } from "../../i18n";
import { SourceHistoryPanel, SourceReadPanel } from "./source-read-panel";

/**
 * Read-only work-source explorer. The workspace UUID comes from workspace
 * context; all server data stays in TanStack Query. Local state holds only
 * the selected source id.
 */
export function SourcesPage() {
  const wsId = useWorkspaceId();
  const { t } = useT("sources");
  const sourcesQuery = useQuery(workSourcesOptions(wsId));
  const { role, isLoading: memberLoading } = useCurrentMember(wsId);
  const canRequest = role === "owner" || role === "admin";

  const sources = useMemo(
    () => sourcesQuery.data ?? [],
    [sourcesQuery.data],
  );
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const selected = useMemo<WorkSource | null>(() => {
    const byId = sources.find((s) => s.id === selectedId);
    return byId ?? sources.find((s) => s.enabled) ?? sources[0] ?? null;
  }, [sources, selectedId]);

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <CollectionPageHeader
        icon={Database}
        title={t(($) => $.title)}
        count={sources.length}
      />
      {sourcesQuery.isLoading || memberLoading ? (
        <div className="flex flex-col gap-2 p-4">
          <Skeleton className="h-5 w-64" />
          <Skeleton className="h-9 w-full max-w-md" />
          <Skeleton className="h-32 w-full" />
        </div>
      ) : sourcesQuery.error ? (
        <CollectionPageState
          icon={TriangleAlert}
          title={t(($) => $.load_error)}
          tone="destructive"
          role="alert"
        />
      ) : sources.length === 0 ? (
        <CollectionPageState
          icon={Database}
          title={t(($) => $.no_sources)}
        />
      ) : (
        <div className="flex flex-col gap-4 p-4">
          <p className="text-body text-muted-foreground">
            {t(($) => $.subtitle)}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <label
              className="text-label shrink-0 font-medium"
              htmlFor="work-source-select"
            >
              {t(($) => $.source_select_label)}
            </label>
            <select
              id="work-source-select"
              className="text-body h-9 max-w-full min-w-0 truncate rounded-md border bg-background px-2 focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring"
              value={selected?.id ?? ""}
              onChange={(event) => setSelectedId(event.target.value)}
            >
              {sources.map((source) => (
                <option key={source.id} value={source.id}>
                  {source.name}
                  {source.enabled ? "" : ` · ${t(($) => $.source_disabled)}`}
                </option>
              ))}
            </select>
            {selected?.project_id ? (
              <span className="text-caption text-muted-foreground truncate">
                {t(($) => $.project_scope)}: {selected.project_id}
              </span>
            ) : null}
          </div>

          {!canRequest ? (
            <p className="text-body text-muted-foreground">
              {t(($) => $.member_no_request)}
            </p>
          ) : null}

          {selected?.mode === "native" ? (
            <p className="text-caption text-muted-foreground" role="status">
              {selected.native_enrollment_status === "enrolled"
                ? t(($) => $.native_status_enrolled)
                : selected.native_enrollment_status === "pending"
                  ? t(($) => $.native_status_pending)
                  : t(($) => $.native_status_unavailable)}{" "}
              {t(($) => $.native_readonly_note)}
            </p>
          ) : null}
          {selected ? (
            selected.workspace_id === wsId ? (
              <div className="flex flex-col gap-6">
                <SourceReadPanel
                  key={`${wsId}:${selected.id}:${selected.config_revision}`}
                  wsId={wsId}
                  source={selected}
                  canRequest={canRequest}
                />
                <SourceHistoryPanel
                  key={`${wsId}:${selected.id}:${selected.config_revision}:history`}
                  wsId={wsId}
                  source={selected}
                />
              </div>
            ) : (
              <p
                className="text-body flex items-center gap-2 text-destructive"
                role="alert"
              >
                <TriangleAlert aria-hidden="true" className="size-4 shrink-0" />
                {t(($) => $.workspace_mismatch)}
              </p>
            )
          ) : null}
        </div>
      )}
    </div>
  );
}
