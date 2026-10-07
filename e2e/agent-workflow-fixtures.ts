import "./env";

import { expect, type Page } from "@playwright/test";
import type { TestAgentWorkflowFixture } from "./fixtures";

const API_BASE = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;

export type WorkflowCommand = {
  id: string;
  kind: "native_session_list" | "native_session_import" | "steer" | "interaction_response";
  runtime_id: string;
  body: Record<string, unknown>;
};

type FakeDaemonCapabilities = {
  nativeImport?: boolean;
};

export type ClaimedWorkflowTask = {
  id: string;
  agent_id: string;
  runtime_id: string;
  chat_session_id: string;
  interaction_mode: string;
  resume_policy: string;
  prior_session_id?: string;
  prior_work_dir?: string;
};

export const nativeSession = {
  native_id: "native-codex-thread-1",
  handle: "/private/codex/thread-1",
  revision: "revision-1",
  title: "Investigate login flow",
  cwd: "/work/e2e-agent-workflow",
  preview: "Investigate the login failure",
  updated_at: "2026-09-19T10:00:00Z",
  model: "gpt-5",
};

/**
 * Test-only daemon transport. It drives the published daemon HTTP contract
 * and deliberately contains no provider executable, account, or transcript
 * file access. Provider behavior is represented only by terminal command
 * reports that the real server persists and exposes to the browser.
 */
export class FakeAgentWorkflowDaemon {
  readonly commands: WorkflowCommand[] = [];
  private readonly claimedCommandIds = new Set<string>();

  constructor(
    readonly fixture: TestAgentWorkflowFixture,
    private readonly capabilities: FakeDaemonCapabilities = {},
  ) {}

  async heartbeat(): Promise<WorkflowCommand[]> {
    const response = await this.daemonRequest("/api/daemon/heartbeat", {
      runtime_id: this.fixture.runtime.id,
      supports_batch_import: true,
      agent_workflow_capabilities: {
        native_sessions: { list: true, import: this.capabilities.nativeImport ?? true },
        controls: { steer: true, approvals: true, questions: true },
      },
    });
    const body = (await response.json()) as { pending_agent_workflow?: WorkflowCommand[] };
    const commands = body.pending_agent_workflow ?? [];
    this.commands.push(...commands);
    return commands;
  }

  async nextCommand(kind: WorkflowCommand["kind"], timeout = 15_000): Promise<WorkflowCommand> {
    await expect
      .poll(
        async () =>
          (await this.heartbeat()).find(
            (command) => command.kind === kind && !this.claimedCommandIds.has(command.id),
          ) ?? null,
        { timeout, intervals: [50, 100, 200, 300] },
      )
      .not.toBeNull();
    const command = this.commands.find(
      (candidate) => candidate.kind === kind && !this.claimedCommandIds.has(candidate.id),
    );
    if (!command) throw new Error(`Fake daemon did not receive ${kind}`);
    this.claimedCommandIds.add(command.id);
    return command;
  }

  async reportResult(command: WorkflowCommand, result: Record<string, unknown>) {
    await this.daemonRequest(
      `/api/daemon/runtimes/${this.fixture.runtime.id}/agent-workflow-requests/${command.id}/result`,
      { status: "completed", result, error: null },
    );
  }

  async reportFailure(command: WorkflowCommand, code: string, message: string) {
    await this.daemonRequest(
      `/api/daemon/runtimes/${this.fixture.runtime.id}/agent-workflow-requests/${command.id}/result`,
      { status: "failed", result: null, error: { code, message } },
    );
  }

  async claimAndStartChatTurn(runId: string) {
    let body: { task: ClaimedWorkflowTask | null } = { task: null };
    await expect.poll(async () => {
      const claim = await this.daemonRequest(
        `/api/daemon/runtimes/${this.fixture.runtime.id}/tasks/claim`,
        undefined,
      );
      const payload: unknown = await claim.json();
      if (!payload || typeof payload !== "object" || !("task" in payload)) {
        throw new Error("Malformed claim response: missing task");
      }
      if (payload.task === null) return null;
      if (typeof payload.task !== "object" || !("id" in payload.task) ||
          typeof payload.task.id !== "string" || !payload.task.id.trim()) {
        throw new Error("Malformed claim response: task requires a nonempty id");
      }
      body = { task: payload.task as ClaimedWorkflowTask };
      return payload.task.id;
    }, { timeout: 15_000, intervals: [50, 100, 200, 300] }).not.toBeNull();
    if (!body.task) throw new Error("Expected queued chat task for fake daemon");
    const turnId = `turn-e2e-${runId}`;
    await this.daemonRequest(`/api/daemon/tasks/${body.task.id}/start`, { run_id: runId });
    await this.reportControlState(body.task.id, {
      run_id: runId,
      turn_id: turnId,
      active: true,
      can_steer: true,
      can_approve: true,
      can_answer: true,
    });
    return { taskId: body.task.id, runId, turnId, task: body.task };
  }

  async reportControlState(taskId: string, state: Record<string, unknown>) {
    await this.daemonRequest(`/api/daemon/tasks/${taskId}/controls`, state);
  }

  async reportInteraction(taskId: string, runId: string, interaction: Record<string, unknown>) {
    await this.daemonRequest(`/api/daemon/tasks/${taskId}/interactions`, {
      run_id: runId,
      interaction,
    });
  }

  async completeChatTurn(taskId: string, output: string) {
    await this.daemonRequest(`/api/daemon/tasks/${taskId}/complete`, { output });
  }

  private async daemonRequest(path: string, body: Record<string, unknown> | undefined): Promise<Response> {
    const response = await fetch(`${API_BASE}${path}`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${this.fixture.runtime.token}`,
        "Content-Type": "application/json",
        "X-Client-Capabilities": "native-session-import-v1,chat-controls-v1",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!response.ok) {
      throw new Error(`fake daemon ${path} failed: ${response.status} ${await response.text()}`);
    }
    return response;
  }
}

export async function openAgentWorkflowChat(
  page: Page,
  fixture: TestAgentWorkflowFixture,
  humanToken: string,
  sessionId?: string,
) {
  await page.addInitScript(
    ({ token, activeSessionId, workspaceSlug }) => {
      localStorage.setItem("multica_token", token);
      localStorage.setItem("multica:chat:isOpen", "false");
      if (activeSessionId) {
        localStorage.setItem(`multica:chat:activeSessionId:${workspaceSlug}`, activeSessionId);
      }
    },
    { token: humanToken, activeSessionId: sessionId, workspaceSlug: fixture.workspace.slug },
  );
  const suffix = sessionId ? `?session=${sessionId}` : "";
  await page.goto(`/${fixture.workspace.slug}/chat${suffix}`, { waitUntil: "domcontentloaded" });
}
