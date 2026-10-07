import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { PropsWithChildren } from "react";
import type { ChatSession } from "@multica/core/types";

const state = vi.hoisted(() => ({
  steer: vi.fn(),
  setMode: vi.fn(),
  operations: {} as Record<string, unknown>,
  requestIds: [] as string[],
}));

vi.mock("@multica/core/chat/queries", () => ({
  workflowCapabilitiesOptions: (_wsId: string, runtimeId: string) => ({
    queryKey: ["workflow-capabilities", runtimeId],
    queryFn: async () => ({
      runtime_id: runtimeId,
      provider: "codex",
      online: true,
      native_sessions: { list: true, import: true },
      controls: { steer: true, approvals: true, questions: true },
      reason: null,
    }),
    enabled: !!runtimeId,
  }),
  chatControlsOptions: (_wsId: string, sessionId: string) => ({
    queryKey: ["workflow-controls", sessionId],
    queryFn: async () => ({
      chat_session_id: sessionId,
      runtime_id: "runtime-1",
      task_id: `task-${sessionId}`,
      run_id: `run-${sessionId}`,
      turn_id: `turn-${sessionId}`,
      active: true,
      can_steer: true,
      can_approve: true,
      can_answer: true,
      interaction_mode: "chat",
      reason: null,
    }),
    enabled: !!sessionId,
  }),
  chatInteractionsOptions: (_wsId: string, sessionId: string) => ({
    queryKey: ["workflow-interactions", sessionId],
    queryFn: async () => ({ items: [] }),
    enabled: !!sessionId,
  }),
  workflowRequestOptions: (_wsId: string, _runtimeId: string, requestId: string) => ({
    queryKey: ["workflow-request", requestId],
    queryFn: async () => state.operations[requestId],
    enabled: !!requestId,
  }),
}));

vi.mock("@multica/core/chat/mutations", () => ({
  useSetChatSessionInteractionMode: () => ({ mutateAsync: state.setMode, isPending: false }),
  useSteerChatSession: () => ({ mutateAsync: state.steer, isPending: false }),
}));

vi.mock("@multica/core/utils", () => ({
  createSafeId: () => state.requestIds.shift() ?? "request-default",
}));

import { useChatWorkflow } from "./use-chat-workflow";

function session(id: string): ChatSession {
  return {
    id,
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: id,
    status: "active",
    has_unread: false,
    created_at: "",
    updated_at: "",
    interaction_mode: "chat",
  };
}

function createWrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    client,
    Wrapper: ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  };
}

beforeEach(() => {
  state.operations = {};
  state.requestIds = [];
  state.steer.mockReset();
  state.setMode.mockReset();
});

describe("useChatWorkflow", () => {
  it("keeps chat A's pending operation while sending in chat B, then restores A's block on return", async () => {
    state.operations["request-a"] = {
      id: "request-a",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "pending",
      result: null,
      error: null,
      created_at: "",
      updated_at: "",
    };
    state.operations["request-b"] = {
      id: "request-b",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "pending",
      result: null,
      error: null,
      created_at: "",
      updated_at: "",
    };
    state.requestIds = ["request-a", "request-b"];
    state.steer.mockImplementation(async ({ requestId }: { requestId: string }) => state.operations[requestId]);
    const { client, Wrapper } = createWrapper();
    const commitA = vi.fn();
    const commitB = vi.fn();
    const { result, rerender } = renderHook(
      ({ current }) => useChatWorkflow({
        wsId: "ws-1",
        session: current,
        runtimeId: "runtime-1",
        visible: true,
      }),
      { initialProps: { current: session("chat-a") }, wrapper: Wrapper },
    );

    await waitFor(() => expect(result.current.canSendNow).toBe(true));
    await act(async () => {
      expect(await result.current.handleSendNow("send A now", undefined, commitA, [])).toBe(false);
    });
    await waitFor(() => expect(result.current.steerOperation?.status).toBe("pending"));

    rerender({ current: session("chat-b") });
    await waitFor(() => {
      expect(result.current.canSendNow).toBe(true);
      expect(result.current.steerOperation).toBeUndefined();
    });

    await act(async () => {
      expect(await result.current.handleSendNow("send B now", undefined, commitB, [])).toBe(false);
    });
    await waitFor(() => expect(result.current.steerOperation?.id).toBe("request-b"));

    rerender({ current: session("chat-a") });
    await waitFor(() => {
      expect(result.current.steerOperation?.id).toBe("request-a");
      expect(result.current.canSendNow).toBe(false);
    });
    expect(state.steer).toHaveBeenCalledTimes(2);
    expect(state.steer).toHaveBeenNthCalledWith(1, expect.objectContaining({
      sessionId: "chat-a",
      requestId: "request-a",
    }));
    expect(state.steer).toHaveBeenNthCalledWith(2, expect.objectContaining({
      sessionId: "chat-b",
      requestId: "request-b",
    }));

    state.operations["request-a"] = {
      id: "request-a",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "completed",
      result: { delivery: "accepted", message_id: "message-a" },
      error: null,
      created_at: "",
      updated_at: "",
    };
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["workflow-request", "request-a"] });
    });

    await waitFor(() => expect(commitA).toHaveBeenCalledTimes(1));
    expect(commitB).not.toHaveBeenCalled();
  });

  it("reconciles the preallocated request after a lost POST acknowledgement without sending it again", async () => {
    state.requestIds = ["request-lost-ack"];
    state.operations["request-lost-ack"] = {
      id: "request-lost-ack",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "pending",
      result: null,
      error: null,
      created_at: "",
      updated_at: "",
    };
    state.steer.mockRejectedValueOnce(new Error("connection reset after request body upload"));
    const { client, Wrapper } = createWrapper();
    const commitInput = vi.fn();
    const { result } = renderHook(
      () => useChatWorkflow({
        wsId: "ws-1",
        session: session("chat-a"),
        runtimeId: "runtime-1",
        visible: true,
      }),
      { wrapper: Wrapper },
    );
    await waitFor(() => expect(result.current.canSendNow).toBe(true));

    await act(async () => {
      await expect(result.current.handleSendNow("preserve this draft", undefined, commitInput, [])).resolves.toBe(false);
    });

    await waitFor(() => {
      expect(result.current.steerOperation?.id).toBe("request-lost-ack");
      expect(result.current.steerOperation?.status).toBe("pending");
      expect(result.current.canSendNow).toBe(false);
      expect(result.current.steerError).toBe("delivery_unknown");
    });
    expect(state.steer).toHaveBeenCalledTimes(1);
    expect(state.steer).toHaveBeenCalledWith(expect.objectContaining({
      requestId: "request-lost-ack",
    }));

    state.operations["request-lost-ack"] = {
      id: "request-lost-ack",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "completed",
      result: { delivery: "accepted", message_id: "message-a" },
      error: null,
      created_at: "",
      updated_at: "",
    };
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["workflow-request", "request-lost-ack"] });
    });

    await waitFor(() => expect(commitInput).toHaveBeenCalledTimes(1));
    expect(state.steer).toHaveBeenCalledTimes(1);
  });

  it("does not send a second steering request before the first mutation settles", async () => {
    let resolveRequest!: (value: unknown) => void;
    const pendingRequest = new Promise<unknown>((resolve) => {
      resolveRequest = resolve;
    });
    state.steer.mockReturnValue(pendingRequest);
    const { Wrapper } = createWrapper();
    const { result } = renderHook(
      () => useChatWorkflow({
        wsId: "ws-1",
        session: session("chat-a"),
        runtimeId: "runtime-1",
        visible: true,
      }),
      { wrapper: Wrapper },
    );
    await waitFor(() => expect(result.current.canSendNow).toBe(true));

    const first = result.current.handleSendNow("first", undefined, vi.fn(), []);
    await expect(result.current.handleSendNow("second", undefined, vi.fn(), [])).resolves.toBe(false);
    expect(state.steer).toHaveBeenCalledTimes(1);

    resolveRequest({
      id: "request-a",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "pending",
      result: null,
      error: null,
      created_at: "",
      updated_at: "",
    });
    await first;
  });

  it("keeps an attachment-bearing send-now draft and reports that the endpoint cannot carry it", async () => {
    const { Wrapper } = createWrapper();
    const { result } = renderHook(
      () => useChatWorkflow({
        wsId: "ws-1",
        session: session("chat-a"),
        runtimeId: "runtime-1",
        visible: true,
      }),
      { wrapper: Wrapper },
    );
    await waitFor(() => expect(result.current.canSendNow).toBe(true));

    await act(async () => {
      await expect(result.current.handleSendNow(
        "attached",
        ["attachment-1"],
        vi.fn(),
        [],
      )).resolves.toBe(false);
    });

    expect(state.steer).not.toHaveBeenCalled();
    await waitFor(() => expect(result.current.steerError).toBe("attachments_unsupported"));
  });

  it("keeps Send now blocked after an uncertain completed delivery", async () => {
    state.operations["request-unknown"] = {
      id: "request-unknown",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "steer",
      status: "completed",
      result: { delivery: "unknown", message_id: null },
      error: null,
      created_at: "",
      updated_at: "",
    };
    state.steer.mockResolvedValue(state.operations["request-unknown"]);
    const { Wrapper } = createWrapper();
    const { result } = renderHook(
      () => useChatWorkflow({
        wsId: "ws-1",
        session: session("chat-a"),
        runtimeId: "runtime-1",
        visible: true,
      }),
      { wrapper: Wrapper },
    );
    await waitFor(() => expect(result.current.canSendNow).toBe(true));

    await act(async () => {
      await expect(result.current.handleSendNow("uncertain", undefined, vi.fn(), [])).resolves.toBe(false);
    });

    await waitFor(() => {
      expect(result.current.steerError).toBe("delivery_unknown");
      expect(result.current.canSendNow).toBe(false);
    });
  });

  it("catches and surfaces an interaction-mode update failure", async () => {
    state.setMode.mockRejectedValueOnce(new Error("stale mode"));
    const { Wrapper } = createWrapper();
    const { result } = renderHook(
      () => useChatWorkflow({
        wsId: "ws-1",
        session: session("chat-a"),
        runtimeId: "runtime-1",
        visible: true,
      }),
      { wrapper: Wrapper },
    );
    await waitFor(() => expect(result.current.canUseChatMode).toBe(true));

    await act(async () => {
      await expect(result.current.setInteractionMode("autonomous")).resolves.toBe(false);
    });
    expect(result.current.modeError).toBe(true);
  });
});
