import { beforeEach, describe, expect, it, vi } from "vitest";
import { StrictMode } from "react";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const state = vi.hoisted(() => {
  const fetchSessions = vi.fn();
  return {
    list: vi.fn(),
    import: vi.fn(),
    fetchSessions,
    queryClient: { fetchQuery: fetchSessions },
    operations: {} as Record<string, unknown>,
    importEnabled: true,
  };
});

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey: unknown[] }) => {
    const [kind, id] = options.queryKey;
    if (kind === "workflow-capabilities") {
      return {
        data: {
          runtime_id: id,
          provider: "codex",
          online: true,
          native_sessions: { list: true, import: state.importEnabled },
          controls: { steer: true, approvals: true, questions: true },
          reason: null,
        },
      };
    }
    if (kind === "workflow-request") return { data: state.operations[String(id)] };
    return {
      data: [{
        id: "runtime-1",
        name: "E2E fake Codex daemon",
        custom_name: null,
        provider: "codex",
      }],
    };
  },
  useQueries: ({ queries }: { queries: Array<{ queryKey: unknown[] }> }) =>
    queries.map((query) => ({ data: state.operations[String(query.queryKey[1])] })),
  useQueryClient: () => state.queryClient,
}));

vi.mock("@multica/core/chat/queries", () => ({
  chatSessionsOptions: (wsId: string) => ({ queryKey: ["chat", wsId, "sessions"] }),
  workflowCapabilitiesOptions: (_wsId: string, runtimeId: string) => ({
    queryKey: ["workflow-capabilities", runtimeId],
  }),
  workflowRequestOptions: (_wsId: string, _runtimeId: string, requestId: string) => ({
    queryKey: ["workflow-request", requestId],
  }),
}));

vi.mock("@multica/core/chat/mutations", () => ({
  useListNativeSessions: () => ({ mutateAsync: state.list, isPending: false }),
  useImportNativeSession: () => ({ mutateAsync: state.import, isPending: false }),
}));

vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: () => ({ queryKey: ["runtimes"] }),
  runtimeDisplayLabel: (runtime: { name: string }) => runtime.name,
}));

import { NativeHistoryImportDialog } from "./native-history-import-dialog";

const agent = {
  id: "agent-1",
  runtime_id: "runtime-1",
  name: "E2E agent",
} as Agent;

function renderDialog(onImported = vi.fn(), strict = false) {
  const dialog = (
    <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
      <NativeHistoryImportDialog wsId="ws-1" agents={[agent]} onImported={onImported} />
    </I18nProvider>
  );
  const view = render(dialog, strict ? { wrapper: StrictMode } : undefined);
  return { ...view, dialog, onImported };
}

async function selectRuntime() {
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: enChat.workflow.import_history }));
  await user.click(screen.getByRole("combobox", { name: enChat.workflow.runtime }));
  await user.click(await screen.findByRole("option", { name: "E2E fake Codex daemon" }));
  return user;
}

beforeEach(() => {
  state.operations = {
    "list-1": {
      id: "list-1",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "native_session_list",
      status: "completed",
      result: {
        sessions: [{
          session_ref: "opaque-ref",
          revision: "revision-1",
          provider: "codex",
          title: "Investigate login flow",
          cwd: "/work/project",
          preview: "Investigate the login failure",
          updated_at: "",
          model: null,
          imported_chat_session_id: null,
        }],
        next_cursor: null,
        truncated: false,
      },
      error: null,
      created_at: "",
      updated_at: "",
    },
    "import-1": {
      id: "import-1",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "native_session_import",
      status: "pending",
      result: null,
      error: null,
      created_at: "",
      updated_at: "",
    },
  };
  state.importEnabled = true;
  state.list.mockReset();
  state.list.mockResolvedValue(state.operations["list-1"]);
  state.import.mockReset();
  state.import.mockResolvedValue(state.operations["import-1"]);
  state.fetchSessions.mockReset();
  state.fetchSessions.mockResolvedValue([{ id: "chat-session-1" }]);
});

describe("NativeHistoryImportDialog", () => {
  it("requires explicit runtime, source, and destination selections before importing", async () => {
    renderDialog();
    const user = await selectRuntime();

    await waitFor(() => expect(screen.getByRole("radio", { name: /Investigate login flow/ })).toBeInTheDocument());
    expect(state.list).toHaveBeenCalledTimes(1);
    const importButton = screen.getByRole("button", { name: enChat.workflow.import_action });
    expect(importButton).toBeDisabled();

    await user.click(screen.getByRole("radio", { name: /Investigate login flow/ }));
    await user.click(screen.getByRole("combobox", { name: enChat.workflow.destination_agent }));
    await user.click(await screen.findByRole("option", { name: agent.name }));
    expect(importButton).toBeEnabled();
    await user.click(importButton);

    await waitFor(() => expect(state.import).toHaveBeenCalledWith({
      runtimeId: "runtime-1",
      sessionRef: "opaque-ref",
      revision: "revision-1",
      agentId: "agent-1",
    }));
  });

  it("shows browse-only history but never enables Import", async () => {
    state.importEnabled = false;
    renderDialog();
    const user = await selectRuntime();

    await waitFor(() => expect(screen.getByText("Investigate login flow")).toBeInTheDocument());
    await user.click(screen.getByRole("radio", { name: /Investigate login flow/ }));
    await user.click(screen.getByRole("combobox", { name: enChat.workflow.destination_agent }));
    await user.click(await screen.findByRole("option", { name: agent.name }));

    expect(screen.getByText(enChat.workflow.browse_only)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: enChat.workflow.import_action })).toBeDisabled();
    expect(state.import).not.toHaveBeenCalled();
  });

  it.each([false, true])("reports an imported chat once through the latest callback after a pending refresh (StrictMode=%s)", async (strict) => {
    let resolveSessions!: (sessions: { id: string }[]) => void;
    state.fetchSessions.mockReturnValueOnce(new Promise<{ id: string }[]>((resolve) => {
      resolveSessions = resolve;
    }));
    const { onImported, rerender } = renderDialog(vi.fn(), strict);
    const user = await selectRuntime();

    await waitFor(() => expect(screen.getByRole("radio", { name: /Investigate login flow/ })).toBeInTheDocument());
    await user.click(screen.getByRole("radio", { name: /Investigate login flow/ }));
    await user.click(screen.getByRole("combobox", { name: enChat.workflow.destination_agent }));
    await user.click(await screen.findByRole("option", { name: agent.name }));
    await user.click(screen.getByRole("button", { name: enChat.workflow.import_action }));

    expect(onImported).not.toHaveBeenCalled();
    state.operations["import-1"] = {
      id: "import-1",
      runtime_id: "runtime-1",
      provider: "codex",
      kind: "native_session_import",
      status: "completed",
      result: {
        chat_session_id: "chat-session-1",
        already_imported: false,
        warnings: [],
      },
      error: null,
      created_at: "",
      updated_at: "",
    };
    rerender(
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <NativeHistoryImportDialog wsId="ws-1" agents={[agent]} onImported={onImported} />
      </I18nProvider>,
    );

    await waitFor(() => expect(state.fetchSessions).toHaveBeenCalledTimes(1));
    expect(onImported).not.toHaveBeenCalled();
    const latestOnImported = vi.fn();
    rerender(
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <NativeHistoryImportDialog wsId="ws-1" agents={[agent]} onImported={latestOnImported} />
      </I18nProvider>,
    );
    resolveSessions([{ id: "chat-session-1" }]);

    await waitFor(() => expect(latestOnImported).toHaveBeenCalledWith("chat-session-1"));
    expect(latestOnImported).toHaveBeenCalledTimes(1);
    expect(onImported).not.toHaveBeenCalled();
    expect(state.fetchSessions).toHaveBeenCalledTimes(1);
  });

  it("keeps choices visible and reports a failed import request", async () => {
    state.import.mockRejectedValueOnce(new Error("network error"));
    renderDialog();
    const user = await selectRuntime();

    await waitFor(() => expect(screen.getByRole("radio", { name: /Investigate login flow/ })).toBeInTheDocument());
    await user.click(screen.getByRole("radio", { name: /Investigate login flow/ }));
    await user.click(screen.getByRole("combobox", { name: enChat.workflow.destination_agent }));
    await user.click(await screen.findByRole("option", { name: agent.name }));
    await user.click(screen.getByRole("button", { name: enChat.workflow.import_action }));

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(enChat.workflow.operation_failed));
    expect(screen.getByRole("radio", { name: /Investigate login flow/ })).toBeChecked();
    expect(screen.getByRole("combobox", { name: enChat.workflow.destination_agent })).toHaveTextContent(agent.name);
  });
});
