/**
 * Work sources: scoped bindings to an external native work-tracking source
 * (e.g. a Beads project), owned by an agent runtime. Read-only "observe"
 * bindings whose identity (workspace, optional project scope, runtime/daemon
 * owner, source handle) is immutable after creation; only name and the
 * enabled flag may change.
 */

export interface WorkSource {
  id: string;
  workspace_id: string;
  /** Omitted when the source is workspace-scoped rather than project-scoped. */
  project_id?: string;
  runtime_id: string;
  /** Opaque daemon identity of the owning runtime; not a UUID. */
  daemon_id: string;
  name: string;
  /** Pinned server-side to "observe" by the current contract. */
  mode: string;
  enabled: boolean;
  /** Opaque approved handle; identity, immutable, whitespace-significant. */
  source_handle: string;
  config_revision: number;
  last_health?: string;
  last_error?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

/**
 * An issue linked to one native work item owned by a work source. The link
 * references both sides by UUID plus the opaque native id; it never creates
 * or deletes either side.
 */
export interface IssueWorkLink {
  id: string;
  workspace_id: string;
  issue_id: string;
  source_id: string;
  /** Opaque native item id; identity, whitespace-significant. */
  native_id: string;
  created_by?: string;
  created_at: string;
}

export interface CreateWorkSourceParams {
  runtime_id: string;
  name: string;
  source_handle: string;
  /** Omit for a workspace-scoped source. */
  project_id?: string;
}

export interface UpdateWorkSourceParams {
  name: string;
  /** Omit to keep the stored flag (a rename can never silently re-enable). */
  enabled?: boolean;
}

export interface CreateIssueWorkLinkParams {
  issue_id: string;
  source_id: string;
  native_id: string;
}
