import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatControls, ChatInteraction, WorkflowCapabilities } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const respondMutateAsync = vi.fn();

vi.mock("@multica/core/chat/mutations", () => ({
  useRespondToChatInteraction: () => ({ mutateAsync: respondMutateAsync, isPending: false }),
}));

vi.mock("@multica/core/chat/queries", () => ({
  workflowRequestOptions: () => ({ queryKey: ["workflow-request"], queryFn: async () => undefined, enabled: false }),
}));

import { ChatInteractionPanel } from "./chat-interaction-panel";

const capabilities: WorkflowCapabilities = {
  runtime_id: "runtime-1",
  provider: "codex",
  online: true,
  native_sessions: { list: true, import: true },
  controls: { steer: true, approvals: true, questions: true },
  reason: null,
};

const controls: ChatControls = {
  chat_session_id: "chat-1",
  runtime_id: "runtime-1",
  task_id: "task-1",
  run_id: "run-1",
  turn_id: "turn-1",
  active: true,
  can_steer: true,
  can_approve: true,
  can_answer: true,
  interaction_mode: "chat",
  reason: null,
};

function interaction(overrides: Partial<ChatInteraction> = {}): ChatInteraction {
  return {
    id: "interaction-1",
    chat_session_id: "chat-1",
    task_id: "task-1",
    run_id: "run-1",
    turn_id: "turn-1",
    kind: "approval",
    status: "pending",
    version: 1,
    title: "Run command?",
    description: "",
    tool: "shell",
    input: { action: "Run command", command: "pnpm test", path: "/repo" },
    choices: [],
    questions: [],
    expires_at: "",
    ...overrides,
  };
}

function renderPanel(interactions: ChatInteraction[]) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <ChatInteractionPanel
          wsId="ws-1"
          sessionId="chat-1"
          controls={controls}
          capabilities={capabilities}
          interactions={interactions}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  respondMutateAsync.mockReset();
  respondMutateAsync.mockResolvedValue({
    id: "request-1",
    runtime_id: "runtime-1",
    provider: "codex",
    kind: "interaction_response",
    status: "pending",
    result: null,
    error: null,
    created_at: "",
    updated_at: "",
  });
});

describe("ChatInteractionPanel", () => {
  it("shows approval action, command, and path before an explicit allow-once response", async () => {
    renderPanel([interaction()]);

    expect(screen.getAllByText("shell").length).toBeGreaterThan(0);
    expect(screen.getByText("action: Run command")).toBeInTheDocument();
    expect(screen.getByText("command: pnpm test")).toBeInTheDocument();
    expect(screen.getByText("path: /repo")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: enChat.workflow.interaction.allow_once }));
    await waitFor(() => expect(respondMutateAsync).toHaveBeenCalledWith(expect.objectContaining({
      sessionId: "chat-1",
      interactionId: "interaction-1",
      taskId: "task-1",
      runId: "run-1",
      turnId: "turn-1",
      response: { choice_id: "allow_once" },
    })));
  });

  it("requires each question to have one response branch and clears selected options for text", async () => {
    renderPanel([interaction({
      kind: "question",
      title: "Choose a test scope",
      questions: [{
        id: "scope",
        prompt: "Which tests should run?",
        options: [{ id: "focused", label: "Focused", description: "Changed package" }],
        multiple: false,
        allow_text: true,
        secret: false,
      }],
    })]);

    const submit = screen.getByRole("button", { name: enChat.workflow.interaction.submit });
    expect(submit).toBeDisabled();
    await userEvent.click(screen.getByRole("radio", { name: /Focused/ }));
    expect(submit).toBeEnabled();

    const text = screen.getByRole("textbox", { name: enChat.workflow.interaction.optional_text });
    fireEvent.change(text, { target: { value: "Run the release suite" } });
    await userEvent.click(submit);

    await waitFor(() => expect(respondMutateAsync).toHaveBeenCalledWith(expect.objectContaining({
      response: {
        answers: [{ question_id: "scope", option_ids: [], text: "Run the release suite" }],
      },
    })));
  });

  it("does not expose a response form for secret input", () => {
    renderPanel([interaction({
      kind: "question",
      questions: [{
        id: "secret",
        prompt: "Enter token",
        options: [],
        multiple: false,
        allow_text: true,
        secret: true,
      }],
    })]);

    expect(screen.getByText(enChat.workflow.interaction.secret_unsupported)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: enChat.workflow.interaction.submit })).not.toBeInTheDocument();
  });

  it("does not allow a second response while delivery is resolving", async () => {
    renderPanel([interaction({ status: "resolving" })]);

    const allow = screen.getByRole("button", { name: enChat.workflow.interaction.allow_once });
    expect(allow).toBeDisabled();
    await userEvent.click(allow);
    expect(respondMutateAsync).not.toHaveBeenCalled();
  });
});
