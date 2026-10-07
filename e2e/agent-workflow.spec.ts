import "./env";

import { randomUUID } from "node:crypto";
import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import { expect, test } from "@playwright/test";
import type { Page, TestInfo } from "@playwright/test";
import {
  FakeAgentWorkflowDaemon,
  nativeSession,
  openAgentWorkflowChat,
  type WorkflowCommand,
} from "./agent-workflow-fixtures";
import { TestApiClient, type TestAgentWorkflowFixture } from "./fixtures";

type WorkflowRequest = {
  id: string;
  status: string;
  result: Record<string, unknown> | null;
  error: { code: string; message: string } | null;
};

async function captureWorkflowScreenshot(page: Page, testInfo: TestInfo, name: string) {
  const directory = process.env.PLAYWRIGHT_SCREENSHOT_DIR;
  if (directory) await mkdir(directory, { recursive: true });
  const path = directory ? join(directory, `${name}.png`) : testInfo.outputPath(`${name}.png`);
  await page.screenshot({ path, fullPage: true });
  await testInfo.attach(name, { path, contentType: "image/png" });
}

async function requestJSON<T>(api: TestApiClient, path: string, init?: RequestInit): Promise<T> {
  const response = await api.request(path, init);
  if (!response.ok) {
    throw new Error(`${init?.method ?? "GET"} ${path} failed: ${response.status} ${await response.text()}`);
  }
  return response.json() as Promise<T>;
}

async function waitForOperation(
  api: TestApiClient,
  runtimeId: string,
  requestId: string,
): Promise<WorkflowRequest> {
  let operation: WorkflowRequest | null = null;
  await expect
    .poll(
      async () => {
        operation = await requestJSON<WorkflowRequest>(
          api,
          `/api/runtimes/${runtimeId}/agent-workflow-requests/${requestId}`,
        );
        return operation.status;
      },
      { timeout: 15_000, intervals: [50, 100, 200, 300] },
    )
    .toMatch(/^(completed|failed|unknown)$/);
  if (!operation) throw new Error(`workflow request ${requestId} was never returned`);
  return operation;
}

async function commandForNativeList(
  api: TestApiClient,
  daemon: FakeAgentWorkflowDaemon,
): Promise<{ requestId: string; command: WorkflowCommand }> {
  const requestId = randomUUID();
  await requestJSON<WorkflowRequest>(api, `/api/runtimes/${daemon.fixture.runtime.id}/native-sessions/list`, {
    method: "POST",
    body: JSON.stringify({ request_id: requestId, cursor: null, limit: 20 }),
  });
  return { requestId, command: await daemon.nextCommand("native_session_list") };
}

test.describe("Paseo-like agent workflow", () => {
  let api: TestApiClient;
  let fixture: TestAgentWorkflowFixture;
  let daemon: FakeAgentWorkflowDaemon;

  test.beforeEach(async ({}, testInfo) => {
    api = new TestApiClient();
    fixture = await api.createAgentWorkflowFixture(
      `${Date.now().toString(36)}-${process.pid.toString(36)}-${testInfo.retry}`,
    );
    daemon = new FakeAgentWorkflowDaemon(fixture);
    await daemon.heartbeat();
  });

  test.afterEach(async ({ page }, testInfo) => {
    try {
      if (testInfo.status !== testInfo.expectedStatus && !page.isClosed()) {
        await captureWorkflowScreenshot(page, testInfo, `failure-${testInfo.testId.replace(/[^a-z0-9]/gi, "")}`);
      }
    } finally {
      await api?.cleanup();
    }
  });

  test("imports a native history through the browser and deduplicates its original source identity", async ({ page }, testInfo) => {
    const token = api.getToken();
    if (!token) throw new Error("agent-workflow fixture did not authenticate the human browser");
    await openAgentWorkflowChat(page, fixture, token);

    await page.getByRole("button", { name: "Import native chat" }).click();
    await page.getByRole("combobox", { name: "Runtime", exact: true }).click();
    await page.getByRole("option", { name: /E2E fake Codex daemon/ }).click();
    const list = await daemon.nextCommand("native_session_list");
    await daemon.reportResult(list, { sessions: [nativeSession], next_cursor: null, truncated: false });
    const completedList = await waitForOperation(api, fixture.runtime.id, list.id);
    expect(completedList.result).toMatchObject({
      sessions: [expect.objectContaining({ title: nativeSession.title })],
      next_cursor: null,
      truncated: false,
    });

    await expect(page.getByText(nativeSession.title, { exact: true })).toBeVisible();
    await expect(page.getByText(nativeSession.handle, { exact: true })).toHaveCount(0);
    await page.getByText(nativeSession.title, { exact: true }).click();
    await page.getByRole("combobox", { name: "Destination agent" }).click();
    await page.getByRole("option", { name: fixture.agent.name }).click();
    await page.getByRole("button", { name: "Import chat" }).click();

    const imported = await daemon.nextCommand("native_session_import");
    expect(imported.body).toMatchObject({
      import_id: expect.any(String),
      handle: nativeSession.handle,
      revision: nativeSession.revision,
      native_id: nativeSession.native_id,
      agent_id: fixture.agent.id,
      chat_session_id: expect.any(String),
    });
    await daemon.reportResult(imported, {
      native_id: nativeSession.native_id,
      owned_native_id: "owned-codex-thread-1",
      provider: "codex",
      resume_session_id: "owned-codex-thread-1",
      work_dir: nativeSession.cwd,
      messages: [
        {
          native_id: "message-user-1",
          role: "user",
          content: "Please investigate the login failure.",
          created_at: "2026-09-19T09:59:00Z",
          events: [],
        },
        {
          native_id: "message-assistant-1",
          role: "assistant",
          content: "I found the authentication boundary.",
          created_at: "2026-09-19T10:00:00Z",
          events: [
            {
              seq: 1,
              type: "tool_use",
              tool: "rg",
              input: { description: "Search authentication handlers", command: "rg authentication server" },
            },
            {
              seq: 2,
              type: "tool_result",
              tool: "rg",
              output: "server/internal/handler/auth.go: authentication boundary",
              output_truncated: false,
            },
          ],
        },
      ],
      warnings: null,
    });
    const completedImport = await waitForOperation(api, fixture.runtime.id, imported.id);
    expect(completedImport).toMatchObject({
      status: "completed",
      result: { chat_session_id: imported.body.chat_session_id, warnings: [] },
    });

    await expect(page).toHaveURL(new RegExp(`/${fixture.workspace.slug}/chat\\?session=`));
    await expect(page.getByTestId("virtuoso-item-list").getByText("I found the authentication boundary.", { exact: true })).toBeVisible();
    await page.getByTestId("virtuoso-item-list").getByRole("button", { name: "2 steps", exact: true }).click();
    await expect(page.getByText("Search authentication handlers", { exact: true })).toBeVisible();
    await captureWorkflowScreenshot(page, testInfo, "imported-thread");

    const sessions = await requestJSON<Array<{ id: string; title: string }>>(api, "/api/chat/sessions");
    expect(sessions.filter((session) => session.title === nativeSession.title)).toHaveLength(1);

    // The successful report distinguishes the source from its owned clone.
    // Replaying the browser-visible original source
    // must still return the existing chat and must not dispatch another clone.
    const replayList = await commandForNativeList(api, daemon);
    await daemon.reportResult(replayList.command, { sessions: [nativeSession], next_cursor: null, truncated: false });
    const replayPage = await waitForOperation(api, fixture.runtime.id, replayList.requestId);
    const replayRef = (replayPage.result?.sessions as Array<{ session_ref: string }> | undefined)?.[0]?.session_ref;
    if (!replayRef) throw new Error("replay native list did not expose a browser session reference");
    const replayImport = await api.request(`/api/runtimes/${fixture.runtime.id}/native-sessions/import`, {
      method: "POST",
      body: JSON.stringify({
        request_id: randomUUID(),
        session_ref: replayRef,
        revision: nativeSession.revision,
        agent_id: fixture.agent.id,
      }),
    });
    expect(replayImport.status).toBe(200);
    expect(await replayImport.json()).toMatchObject({
      status: "completed",
      result: { chat_session_id: imported.body.chat_session_id, already_imported: true },
    });
    await daemon.heartbeat();
    expect(daemon.commands.filter((command) => command.kind === "native_session_import")).toHaveLength(1);

    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("virtuoso-item-list").getByText("I found the authentication boundary.", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(page.locator(".ProseMirror").last()).toBeVisible({ timeout: 30_000 });
    await page.locator(".ProseMirror").last().fill("Continue the owned conversation");
    const resumeSend = page.waitForResponse((response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/chat/sessions/${imported.body.chat_session_id}/messages`,
    );
    await page.getByRole("button", { name: /^Send(?: message)?$/, exact: true }).click();
    expect((await resumeSend).ok()).toBe(true);
    const resumed = await daemon.claimAndStartChatTurn(randomUUID());
    expect(resumed.task).toMatchObject({
      chat_session_id: imported.body.chat_session_id,
      agent_id: fixture.agent.id,
      runtime_id: fixture.runtime.id,
      interaction_mode: "chat",
      resume_policy: "require_native",
      prior_session_id: "owned-codex-thread-1",
      prior_work_dir: nativeSession.cwd,
    });
  });

  test("rolls back a failed import before a chat becomes visible", async () => {
    const { requestId, command } = await commandForNativeList(api, daemon);
    await daemon.reportResult(command, {
      sessions: [{ ...nativeSession, title: "Changed before import", revision: "revision-before-change" }],
      next_cursor: null,
      truncated: false,
    });
    const listed = await waitForOperation(api, fixture.runtime.id, requestId);
    const sessionRef = (listed.result?.sessions as Array<{ session_ref: string }> | undefined)?.[0]?.session_ref;
    if (!sessionRef) throw new Error("native list result did not expose a browser session reference");

    const importId = randomUUID();
    await requestJSON<WorkflowRequest>(api, `/api/runtimes/${fixture.runtime.id}/native-sessions/import`, {
      method: "POST",
      body: JSON.stringify({
        request_id: importId,
        session_ref: sessionRef,
        revision: "revision-before-change",
        agent_id: fixture.agent.id,
      }),
    });
    await daemon.reportFailure(await daemon.nextCommand("native_session_import"), "source_changed", "Native source changed during copy");
    const completed = await waitForOperation(api, fixture.runtime.id, importId);
    expect(completed).toMatchObject({ status: "failed", error: { code: "source_changed" } });

    const sessions = await requestJSON<Array<{ title: string }>>(api, "/api/chat/sessions");
    expect(sessions.some((session) => session.title === "Changed before import")).toBe(false);
  });

  test("shows browse-only native history without allowing an unsupported import", async ({ page }) => {
    const browseOnlyDaemon = new FakeAgentWorkflowDaemon(fixture, { nativeImport: false });
    await browseOnlyDaemon.heartbeat();
    const token = api.getToken();
    if (!token) throw new Error("agent-workflow fixture did not authenticate the human browser");
    await openAgentWorkflowChat(page, fixture, token);

    await page.getByRole("button", { name: "Import native chat" }).click();
    await page.getByRole("combobox", { name: "Runtime", exact: true }).click();
    await page.getByRole("option", { name: /E2E fake Codex daemon/ }).click();
    await browseOnlyDaemon.reportResult(await browseOnlyDaemon.nextCommand("native_session_list"), {
      sessions: [nativeSession],
      next_cursor: null,
      truncated: false,
    });

    await expect(page.getByText(nativeSession.title, { exact: true })).toBeVisible();
    await page.getByText(nativeSession.title, { exact: true }).click();
    await page.getByRole("combobox", { name: "Destination agent" }).click();
    await page.getByRole("option", { name: fixture.agent.name }).click();
    await expect(page.getByText("This runtime can browse history but cannot import it.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Import chat" })).toBeDisabled();
    await browseOnlyDaemon.heartbeat();
    expect(browseOnlyDaemon.commands.filter((command) => command.kind === "native_session_import")).toEqual([]);
  });

  test("sends now once, restores pending interactions, and rejects a stale reply", async ({ page }, testInfo) => {
    const session = await requestJSON<{ id: string }>(api, "/api/chat/sessions", {
      method: "POST",
      body: JSON.stringify({ agent_id: fixture.agent.id, title: "Exact foreground turn" }),
    });
    const token = api.getToken();
    if (!token) throw new Error("agent-workflow fixture did not authenticate the human browser");
    await openAgentWorkflowChat(page, fixture, token, session.id);

    const interactiveMode = page.getByRole("radio", { name: "Ask before tools and questions" });
    await interactiveMode.click();
    await expect(interactiveMode).toBeChecked();

    const editor = page.locator(".ProseMirror").last();
    await expect(editor).toBeVisible({ timeout: 30_000 });
    await editor.fill("Queue the initial turn");
    const initialSend = page.waitForResponse((response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/chat/sessions/${session.id}/messages`,
    );
    await page.getByRole("button", { name: /^Send(?: message)?$/, exact: true }).click();
    expect((await initialSend).ok()).toBe(true);
    const active = await daemon.claimAndStartChatTurn(randomUUID());
    expect(active.task).toMatchObject({
      chat_session_id: session.id,
      interaction_mode: "chat",
      resume_policy: "allow_fresh",
    });
    expect(active.task.prior_session_id ?? "").toBe("");

    await editor.fill("Send this exact turn now");
    const messagePosts: string[] = [];
    page.on("request", (request) => {
      if (new URL(request.url()).pathname === `/api/chat/sessions/${session.id}/messages`) {
        messagePosts.push(request.postData() ?? "");
      }
    });
    await page.getByRole("button", { name: "Send now" }).click();
    const steer = await daemon.nextCommand("steer");
    expect(steer.body).toMatchObject({
      chat_session_id: session.id,
      task_id: active.taskId,
      run_id: active.runId,
      turn_id: active.turnId,
      content: "Send this exact turn now",
    });
    await daemon.reportResult(steer, { delivery: "accepted" });
    expect(messagePosts).toEqual([]);

    await editor.fill("Queue a separate follow-up turn");
    const queuedResponse = page.waitForResponse((response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/chat/sessions/${session.id}/messages`,
    );
    await page.getByRole("button", { name: "Queue", exact: true }).click();
    expect((await queuedResponse).ok()).toBe(true);
    expect(messagePosts).toHaveLength(1);
    expect(JSON.parse(messagePosts[0])).toMatchObject({ content: "Queue a separate follow-up turn" });
    const pending = await requestJSON<{ tasks: Array<{ task_id: string; chat_session_id: string; status: string }> }>(api, "/api/chat/pending-tasks");
    expect(pending.tasks.filter((task) => task.chat_session_id === session.id)).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ task_id: active.taskId, status: "running" }),
        expect.objectContaining({ status: "queued" }),
      ]),
    );
    await daemon.heartbeat();
    expect(daemon.commands.filter((command) => command.kind === "steer")).toHaveLength(1);

    const approvalId = randomUUID();
    await daemon.reportInteraction(active.taskId, active.runId, {
      id: approvalId,
      turn_id: active.turnId,
      kind: "approval",
      title: "Run focused tests",
      description: "",
      tool: "shell",
      input: { command: "pnpm test --filter @multica/views" },
      choices: [
        { id: "allow_once", label: "Allow once" },
        { id: "deny", label: "Deny" },
      ],
      questions: [],
      expires_at: "2099-01-01T00:00:00Z",
    });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("button", { name: "Allow once" })).toBeVisible();
    await page.getByRole("button", { name: "Allow once" }).click();
    const approval = await daemon.nextCommand("interaction_response");
    expect(approval.body).toMatchObject({
      interaction_id: approvalId,
      response: { choice_id: "allow_once" },
    });
    await daemon.reportResult(approval, { delivery: "accepted" });

    const questionId = randomUUID();
    await daemon.reportInteraction(active.taskId, active.runId, {
      id: questionId,
      turn_id: active.turnId,
      kind: "question",
      title: "Select a test scope",
      description: "",
      tool: "request_user_input",
      input: {},
      choices: [],
      questions: [
        {
          id: "scope",
          prompt: "Which tests should run?",
          options: [{ id: "focused", label: "Focused tests", description: "Run tests for the changed package." }],
          multiple: false,
          allow_text: true,
          secret: false,
        },
      ],
      expires_at: "2099-01-01T00:00:00Z",
    });
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.getByRole("radio", { name: "Focused tests" }).check();
    await captureWorkflowScreenshot(page, testInfo, "structured-question");
    await page.getByRole("button", { name: "Submit response" }).click();
    const question = await daemon.nextCommand("interaction_response");
    expect(question.body).toMatchObject({
      interaction_id: questionId,
      response: { answers: [{ question_id: "scope", option_ids: ["focused"], text: "" }] },
    });
    await daemon.reportResult(question, { delivery: "accepted" });

    await daemon.reportControlState(active.taskId, {
      run_id: active.runId,
      turn_id: null,
      active: false,
      can_steer: false,
      can_approve: false,
      can_answer: false,
    });
    const stale = await api.request(
      `/api/chat-sessions/${session.id}/interactions/${questionId}/respond`,
      {
        method: "POST",
        body: JSON.stringify({
          request_id: randomUUID(),
          task_id: active.taskId,
          run_id: active.runId,
          turn_id: active.turnId,
          response: { answers: [{ question_id: "scope", option_ids: ["focused"], text: "" }] },
        }),
      },
    );
    expect(stale.status).toBe(409);
    await expect(stale.json()).resolves.toMatchObject({ code: "stale_turn" });
    expect(daemon.commands.filter((command) => command.kind === "interaction_response")).toHaveLength(2);
  });

  test("retires an unanswered question when its run ends and permits steering the next run", async ({ page }, testInfo) => {
    const session = await requestJSON<{ id: string }>(api, "/api/chat/sessions", {
      method: "POST",
      body: JSON.stringify({ agent_id: fixture.agent.id, title: "Question lifecycle", interaction_mode: "chat" }),
    });
    const token = api.getToken();
    if (!token) throw new Error("agent-workflow fixture did not authenticate the human browser");
    await openAgentWorkflowChat(page, fixture, token, session.id);
    const editor = page.locator(".ProseMirror").last();
    await expect(editor).toBeVisible({ timeout: 30_000 });
    await editor.fill("Start the run that will leave a question unanswered");
    const firstSend = page.waitForResponse((response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/chat/sessions/${session.id}/messages`,
    );
    await page.getByRole("button", { name: /^Send(?: message)?$/, exact: true }).click();
    expect((await firstSend).ok()).toBe(true);
    const ended = await daemon.claimAndStartChatTurn(randomUUID());
    const questionId = randomUUID();
    await daemon.reportInteraction(ended.taskId, ended.runId, {
      id: questionId,
      turn_id: ended.turnId,
      kind: "question",
      title: "Unanswered question from the ended run",
      description: "",
      tool: "request_user_input",
      input: {},
      choices: [],
      questions: [{
        id: "scope",
        prompt: "Which scope should this run use?",
        options: [{ id: "focused", label: "This run only", description: "" }],
        multiple: false,
        allow_text: false,
        secret: false,
      }],
      expires_at: "2099-01-01T00:00:00Z",
    });
    await expect(page.getByRole("radio", { name: "This run only" })).toBeVisible({ timeout: 15_000 });
    const interactionsPath = `/api/chat-sessions/${session.id}/interactions`;
    expect(await requestJSON(api, interactionsPath)).toMatchObject({
      items: [expect.objectContaining({ id: questionId, status: "pending", run_id: ended.runId })],
    });
    await editor.fill("This input must wait for the question");
    await expect(page.getByRole("button", { name: "Send now", exact: true })).toBeDisabled();
    await editor.fill("");

    // No reply or synthetic inactive-controls report: task completion itself
    // must retire persisted interactions, even if the daemon disconnects.
    await daemon.completeChatTurn(ended.taskId, "The earlier run has ended.");
    await expect.poll(() => requestJSON(api, interactionsPath)).toEqual({ items: [] });
    await expect(page.getByText("Unanswered question from the ended run", { exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Submit response" })).toHaveCount(0);

    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("virtuoso-item-list").getByText("The earlier run has ended.", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole("radio", { name: "This run only" })).toHaveCount(0);
    await expect(editor).toBeVisible({ timeout: 30_000 });
    await editor.fill("Start a fresh run after that unanswered question");
    const nextSend = page.waitForResponse((response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/chat/sessions/${session.id}/messages`,
    );
    await page.getByRole("button", { name: /^Send(?: message)?$/, exact: true }).click();
    expect((await nextSend).ok()).toBe(true);
    const fresh = await daemon.claimAndStartChatTurn(randomUUID());
    expect(fresh.taskId).not.toBe(ended.taskId);
    expect(fresh.runId).not.toBe(ended.runId);
    expect(fresh.turnId).not.toBe(ended.turnId);
    expect(fresh.task.chat_session_id).toBe(session.id);
    await editor.fill("Steer only the fresh run");
    await expect(page.getByRole("button", { name: "Send now", exact: true })).toBeEnabled({ timeout: 15_000 });
    expect(await requestJSON(api, interactionsPath)).toEqual({ items: [] });
    await expect(page.getByRole("button", { name: "Submit response" })).toHaveCount(0);
    await captureWorkflowScreenshot(page, testInfo, "fresh-run-after-retired-question");
    await page.getByRole("button", { name: "Send now", exact: true }).click();
    const steer = await daemon.nextCommand("steer");
    expect(steer.body).toMatchObject({
      chat_session_id: session.id, task_id: fresh.taskId,
      run_id: fresh.runId, turn_id: fresh.turnId, content: "Steer only the fresh run",
    });
    await daemon.reportResult(steer, { delivery: "accepted" });
    expect((await waitForOperation(api, fixture.runtime.id, steer.id)).status).toBe("completed");
    expect(daemon.commands.filter((command) => command.kind === "interaction_response")).toHaveLength(0);
    expect(daemon.commands.filter((command) => command.kind === "steer")).toHaveLength(1);
  });
});
