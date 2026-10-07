/**
 * Work sources: workspace-scoped native work-tracking bindings (e.g. Beads),
 * owned by an agent runtime. Observe sources are read-only. Native sources
 * require dedicated-domain enrollment before enablement. Binding identity
 * (workspace, project scope, runtime, daemon and handle) is immutable.
 */

/** Native enrollment state reported by the server for "native" mode sources. */
export type WorkSourceNativeEnrollmentStatus =
  | "pending"
  | "enrolled"
  | "unknown";

export interface WorkSource {
  id: string;
  workspace_id: string;
  /** Omitted when the source is workspace-scoped rather than project-scoped. */
  project_id?: string;
  runtime_id: string;
  /** Opaque daemon identity of the owning runtime; not a UUID. */
  daemon_id: string;
  name: string;
  /** "observe" is read-only; "native" requires dedicated enrollment. */
  mode: string;
  enabled: boolean;
  /** Opaque approved handle; identity, immutable, whitespace-significant. */
  source_handle: string;
  config_revision: number;
  last_health?: string;
  last_error?: string;
  /** Present only for "native" mode sources; omitted by observe-only servers. */
  native_enrollment_id?: string;
  /** Unknown/missing server values fall back to "unknown", never "enrolled". */
  native_enrollment_status?: WorkSourceNativeEnrollmentStatus;
  native_manifest_hash?: string;
  native_owner_member_id?: string;
  native_runtime_created_at?: string;
  native_enrolled_at?: string;
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

/** Read-only allowlisted commands a source's daemon may execute. */
export type WorkSourceCommandName = "read" | "list";

export type WorkSourceCommandStatus = "pending" | "claimed" | "succeeded" | "failed" | "unknown";

/**
 * A read-only source command receipt. `request_id` is a caller-supplied UUID
 * making create idempotent; status is terminal only on succeeded/failed.
 */
export interface WorkSourceCommand {
  id: string;
  request_id: string;
  workspace_id: string;
  source_id: string;
  command: WorkSourceCommandName;
  /** Present for "read" only; opaque, whitespace-significant. */
  native_id?: string;
  /** Present for "list" only. */
  limit_count?: number;
  status: WorkSourceCommandStatus;
  config_revision: number;
  expires_at: string;
  claimed_runtime_id?: string;
  claimed_at?: string;
  /** Present only on get/create; list responses strip it. */
  result?: string;
  error?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateWorkSourceCommandParams {
  /** Caller-supplied UUID; an identical retry returns the existing receipt. */
  request_id: string;
  command: WorkSourceCommandName;
  /** Required for "read". */
  native_id?: string;
  /** Only valid for "list". */
  limit?: number;
}
