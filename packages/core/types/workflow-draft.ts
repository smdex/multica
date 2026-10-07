/**
 * Workflow drafts: non-executing frozen observation snapshots derived from
 * trusted work-source read receipts. A draft is a closed, bounded, digested
 * dependency closure plus per-node initial state; there is deliberately no
 * Start API. All native IDs and revisions are opaque source values,
 * whitespace-significant, never normalized here.
 */

export interface WorkflowDraftGraphNode {
  native_id: string;
  revision: string;
  title: string;
  status: string;
  receipt_id: string;
  observed_at: string;
}

export interface WorkflowDraftGraphEdge {
  predecessor_native_id: string;
  consumer_native_id: string;
  /** Draft graphs only ever carry "blocks" edges. */
  dependency_type: "blocks";
}

/** Bounded closure: at most 128 nodes and 512 edges. */
export interface WorkflowDraftGraph {
  workspace_id: string;
  source_id: string;
  root_native_id: string;
  config_revision: number;
  nodes: WorkflowDraftGraphNode[];
  edges: WorkflowDraftGraphEdge[];
  /** SHA256 over the canonical scope/config/root/revisions/topology encoding. */
  digest: string;
}

/**
 * A draft node's only possible state: blocked before review, with the reason
 * always being that the graph is still a draft. An unknown status from a
 * future backend must fail closed (reject the response), never coerce into a
 * runnable state.
 */
export interface WorkflowDraftNodeState {
  status: "blocked";
  reason: "draft";
}

export interface WorkflowDraft {
  id: string;
  workspace_id: string;
  /** Omitted when the owning source is workspace-scoped. */
  project_id?: string;
  source_id: string;
  request_id: string;
  root_native_id: string;
  config_revision: number;
  capacity: number;
  status: "draft";
  graph: WorkflowDraftGraph;
  /** Keys are exact node native_ids from `graph`. */
  node_state: Record<string, WorkflowDraftNodeState>;
  created_by?: string;
  created_at: string;
}

export interface CreateWorkflowDraftParams {
  /** Caller-supplied UUID; an identical retry returns the stored draft (200). */
  request_id: string;
  source_id: string;
  root_native_id: string;
  expected_root_revision: string;
  expected_config_revision: number;
  /** 1..2. */
  capacity: number;
  /** 1..128 receipt UUIDs establishing the root closure. */
  receipt_ids: string[];
}
