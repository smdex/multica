import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { RESOURCES } from "../../locales";
import type {
  CreateWorkSourceCommandParams,
  MemberRole,
  WorkSource,
  WorkSourceCommand,
} from "@multica/core/types";

const mockListWorkSources = vi.hoisted(() => vi.fn());
const mockListMembers = vi.hoisted(() => vi.fn());
const mockCreateWorkSourceCommand = vi.hoisted(() => vi.fn());
const mockGetWorkSourceCommand = vi.hoisted(() => vi.fn());
const mockListWorkSourceCommands = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    listWorkSources: mockListWorkSources,
    listMembers: mockListMembers,
    createWorkSourceCommand: mockCreateWorkSourceCommand,
    getWorkSourceCommand: mockGetWorkSourceCommand,
    listWorkSourceCommands: mockListWorkSourceCommands,
  },
}));
vi.mock("@multica/core/auth", () => {
  const state = { user: { id: "user-1" } };
  const useAuthStore = Object.assign(
    (sel: (s: typeof state) => unknown) => sel(state),
    { getState: () => state },
  );
  return { useAuthStore };
});
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

import { SourcesPage } from "./sources-page";

const WS = "ws-1";

function source(overrides: Partial<WorkSource> = {}): WorkSource {
  return {
    id: "source-1",
    workspace_id: WS,
    runtime_id: "rt-1",
    daemon_id: "daemon-1",
    name: "Beads upstream",
    mode: "observe",
    enabled: true,
    source_handle: "beads:proj",
    config_revision: 3,
    created_at: "2026-10-07T00:00:00Z",
    updated_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

const LIST_ITEM = {
  id: "bd-1",
  title: "First item",
  status: "open",
  priority: 1,
  issue_type: "task",
  dependency_count: 0,
  dependent_count: 0,
};

/** A receipt that echoes the actual request body, so scope checks pass. */
function receiptFor(
  body: CreateWorkSourceCommandParams,
  overrides: Partial<WorkSourceCommand> = {},
): WorkSourceCommand {
  return {
    id: `cmd-${body.request_id}`,
    request_id: body.request_id,
    workspace_id: WS,
    source_id: "source-1",
    command: body.command,
    native_id: body.native_id,
    limit_count: body.limit,
    status: "pending",
    config_revision: 3,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
    created_at: "2026-10-07T00:00:00Z",
    updated_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

const receipts = new Map<string, WorkSourceCommand>();
const history: WorkSourceCommand[] = [];

/** History responses return fresh immutable rows with `result` stripped. */
function historyRows(): WorkSourceCommand[] {
  return history.map((row) => {
    const { result: _result, ...rest } = row;
    return { ...rest };
  });
}

/** GET responses return a fresh copy so later status changes never alias. */
function receiptSnapshot(id: string): WorkSourceCommand | undefined {
  const row = receipts.get(id);
  return row ? { ...row } : undefined;
}

function Wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={RESOURCES}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  receipts.clear();
  history.length = 0;
  mockListWorkSources.mockResolvedValue([source()]);
  mockListMembers.mockResolvedValue([{ user_id: "user-1", role: "owner" }]);
  mockListWorkSourceCommands.mockImplementation(async () => historyRows());
  mockCreateWorkSourceCommand.mockImplementation(
    async ({ body }: { body: CreateWorkSourceCommandParams }) => {
      const rc = receiptFor(body);
      receipts.set(rc.id, rc);
      history.push(rc);
      return rc;
    },
  );
  mockGetWorkSourceCommand.mockImplementation(
    async ({ commandId }: { commandId: string }) =>
      receiptSnapshot(commandId) ??
      Promise.reject(new Error("unknown command")),
  );
});

async function renderPage(role: MemberRole = "owner") {
  mockListMembers.mockResolvedValue([{ user_id: "user-1", role }]);
  render(<SourcesPage />, { wrapper: Wrapper });
  await screen.findByRole("combobox", { name: /source/i });
}

describe("SourcesPage", () => {
  it("creates no read command on mount or source selection", async () => {
    await renderPage();
    await waitFor(() => expect(mockListWorkSources).toHaveBeenCalled());
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();
  });

  it("disables read requests for members but keeps history inspection", async () => {
    await renderPage("member");
    expect(
      screen.getByText(/only owners and admins can request/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /refresh/i }),
    ).toBeDisabled();
    // History panel is present for members.
    expect(
      screen.getByRole("combobox", { name: /existing results/i }),
    ).toBeInTheDocument();
  });

  it("enables read requests for admins", async () => {
    await renderPage("admin");
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /refresh/i }),
      ).toBeEnabled(),
    );
  });

  it("runs an explicit list, then a one-click item read; retry reuses the same body", async () => {
    const user = userEvent.setup();
    await renderPage();

    mockCreateWorkSourceCommand.mockImplementation(
      async ({ body }: { body: CreateWorkSourceCommandParams }) => {
        const rc = receiptFor(body, {
          status: "succeeded",
          result:
            body.command === "list"
              ? JSON.stringify([LIST_ITEM])
              : JSON.stringify({ ...LIST_ITEM, revision: "rev-9" }),
        });
        receipts.set(rc.id, rc);
        history.push(rc);
        return rc;
      },
    );

    await user.click(screen.getByRole("button", { name: /refresh/i }));
    const listBody = mockCreateWorkSourceCommand.mock.calls[0]![0].body;
    expect(listBody.command).toBe("list");
    expect(listBody.limit).toBe(50);
    await screen.findByText("First item");
    expect(
      screen.getByText(/Showing the first 50 items/i),
    ).toBeInTheDocument();

    // One click on the row's Read fires the read command directly.
    await user.click(screen.getByRole("button", { name: /^read$/i }));
    await waitFor(() =>
      expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(2),
    );
    const readBody = mockCreateWorkSourceCommand.mock.calls[1]![0].body;
    expect(readBody.command).toBe("read");
    expect(readBody.native_id).toBe("bd-1");
    await screen.findByText("rev-9");

    // Transport failure: retry must replay the exact same body UUID.
    mockCreateWorkSourceCommand.mockRejectedValueOnce(
      new Error("network down"),
    );
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    const retry = await screen.findByRole("button", { name: /^retry$/i });
    await user.click(retry);
    await waitFor(() =>
      expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(4),
    );
    const failed = mockCreateWorkSourceCommand.mock.calls[2]![0].body;
    const retried = mockCreateWorkSourceCommand.mock.calls[3]![0].body;
    expect(retried).toEqual(failed);
    expect(retried.request_id).toBe(failed.request_id);
  });

  it("shows a failed command as a terminal error without auto-creating a new one", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand.mockImplementation(
      async ({ body }: { body: CreateWorkSourceCommandParams }) => {
        const rc = receiptFor(body, {
          status: "failed",
          error: "daemon offline",
        });
        receipts.set(rc.id, rc);
        history.push(rc);
        return rc;
      },
    );
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    await screen.findByText(/daemon offline/);
    expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(1);
  });

  it("reports malformed succeeded results instead of rendering them", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand.mockImplementation(
      async ({ body }: { body: CreateWorkSourceCommandParams }) => {
        const rc = receiptFor(body, { status: "succeeded", result: "not json" });
        receipts.set(rc.id, rc);
        history.push(rc);
        return rc;
      },
    );
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    await screen.findByText(/failed|did not match/i);
    expect(screen.queryByText("First item")).not.toBeInTheDocument();
  });

  it("offers a manual recheck once the receipt deadline passes", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand.mockImplementation(
      async ({ body }: { body: CreateWorkSourceCommandParams }) => {
        const rc = receiptFor(body, {
          status: "claimed",
          expires_at: new Date(Date.now() - 1000).toISOString(),
        });
        receipts.set(rc.id, rc);
        history.push(rc);
        return rc;
      },
    );
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    const recheck = await screen.findByRole("button", {
      name: /check again/i,
    });
    mockGetWorkSourceCommand.mockClear();
    await user.click(recheck);
    await waitFor(() => expect(mockGetWorkSourceCommand).toHaveBeenCalled());
    // No new command was minted while the outcome was uncertain.
    expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(1);
  });

  it("lets members inspect an existing history receipt without creating", async () => {
    const user = userEvent.setup();
    // Seed history with a previously succeeded list.
    const past = receiptFor(
      { request_id: "hist-1", command: "list", limit: 50 },
      {
        id: "cmd-hist-1",
        status: "succeeded",
        result: JSON.stringify([LIST_ITEM]),
      },
    );
    history.push(past);
    receipts.set(past.id, past);
    await renderPage("member");

    const select = screen.getByRole("combobox", { name: /existing results/i });
    await waitFor(() =>
      expect(
        screen.getByRole("option", { name: /list/i }),
      ).toBeInTheDocument(),
    );
    await user.selectOptions(select, "cmd-hist-1");
    await screen.findAllByText("First item");
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();
  });

  it("blocks creation and stays busy while history holds a pending receipt", async () => {
    const user = userEvent.setup();
    history.push(
      receiptFor({ request_id: "hist-pending", command: "list", limit: 50 }, {
        id: "cmd-hist-pending",
      }),
    );
    await renderPage();
    const refresh = await screen.findByRole("button", { name: /refresh/i });
    await waitFor(() => expect(refresh).toBeDisabled());
    await user.click(refresh).catch(() => undefined);
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();
  });

  it("treats unknown receipts as busy with a manual recheck, never terminal", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand.mockImplementation(
      async ({ body }: { body: CreateWorkSourceCommandParams }) => {
        const rc = receiptFor(body, { status: "unknown" });
        receipts.set(rc.id, rc);
        history.push(rc);
        return rc;
      },
    );
    const refresh = screen.getByRole("button", { name: /refresh/i });
    await user.click(refresh);
    expect(
      await screen.findByText(/outcome of this read is not known/i),
    ).toBeInTheDocument();
    const refreshAfter = screen.getByRole("button", { name: /refresh/i });
    await waitFor(() => expect(refreshAfter).toBeDisabled());
    mockGetWorkSourceCommand.mockClear();
    await user.click(screen.getAllByRole("button", { name: /check again/i })[0]!);
    await waitFor(() => expect(mockGetWorkSourceCommand).toHaveBeenCalled());
    // Unknown never authorizes a new command.
    expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(1);
  });

  it("keeps the last accepted receipt busy on GET error and offers a recheck", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand.mockImplementation(
      async ({ body }: { body: CreateWorkSourceCommandParams }) => {
        const rc = receiptFor(body, { status: "claimed" });
        receipts.set(rc.id, rc);
        history.push(rc);
        return rc;
      },
    );
    // GET fails from the start; the accepted claimed receipt (cached by the
    // create onSuccess) must keep creation fenced, with an explicit recheck.
    mockGetWorkSourceCommand.mockRejectedValue(new Error("get failed"));
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    const refresh = screen.getByRole("button", { name: /refresh/i });
    await waitFor(() => expect(refresh).toBeDisabled());
    expect(
      await screen.findByText(/read receipt did not match/i),
    ).toBeInTheDocument();
    // The recheck recovers the receipt without minting a new command.
    mockGetWorkSourceCommand.mockClear();
    mockGetWorkSourceCommand.mockImplementation(
      async ({ commandId }: { commandId: string }) =>
        receiptSnapshot(commandId) ??
        Promise.reject(new Error("unknown command")),
    );
    const recheck = await screen.findAllByRole("button", { name: /check again/i });
    await user.click(recheck[0]!);
    await waitFor(() => expect(mockGetWorkSourceCommand).toHaveBeenCalled());
    expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(1);
  });

  it("fences creation while history is loading or failed and shows a recoverable failure", async () => {
    const user = userEvent.setup();
    let failHistory = true;
    mockListWorkSourceCommands.mockImplementation(async () => {
      if (failHistory) throw new Error("history down");
      return historyRows();
    });
    await renderPage();
    const refresh = screen.getByRole("button", { name: /refresh/i });
    await waitFor(() => expect(refresh).toBeDisabled());
    expect(
      await screen.findByText(/existing read results could not be loaded/i),
    ).toBeInTheDocument();
    // Manual history recheck recovers and re-enables creation.
    failHistory = false;
    await user.click(
      screen.getByRole("button", { name: /check again/i, hidden: false }),
    );
    await waitFor(() => expect(refresh).toBeEnabled());
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();
  });

  it("adopts a successful retried create into the list pointer with the same intent", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand
      .mockRejectedValueOnce(new Error("network down"))
      .mockImplementationOnce(
        async ({ body }: { body: CreateWorkSourceCommandParams }) => {
          const rc = receiptFor(body, {
            status: "succeeded",
            result: JSON.stringify([LIST_ITEM]),
          });
          receipts.set(rc.id, rc);
          history.push(rc);
          return rc;
        },
      );
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    const retried = mockCreateWorkSourceCommand.mock.calls[0]![0].body;
    await user.click(await screen.findByRole("button", { name: /^retry$/i }));
    await waitFor(() =>
      expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(2),
    );
    expect(mockCreateWorkSourceCommand.mock.calls[1]![0].body).toEqual(retried);
    // The retried command's result is now visible via the adopted pointer.
    await screen.findByText("First item");
  });

  it("releases the creation fence when detail GET confirms terminal despite stale pending history", async () => {
    const user = userEvent.setup();
    await renderPage();
    mockCreateWorkSourceCommand.mockImplementation(async ({ body }: { body: CreateWorkSourceCommandParams }) => {
      const pending = receiptFor(body);
      history.push(pending);
      receipts.set(pending.id, { ...pending, status: "succeeded", result: JSON.stringify([LIST_ITEM]) });
      return pending;
    });
    await user.click(screen.getByRole("button", { name: /refresh/i }));
    await screen.findByText("First item");
    await waitFor(() => expect(screen.getByRole("button", { name: /refresh/i })).toBeEnabled());
    expect(history[0]?.status).toBe("pending");
    expect(mockCreateWorkSourceCommand).toHaveBeenCalledTimes(1);
  });

  it("treats foreign workspace rows in the sources list as a load error", async () => {
    mockListWorkSources.mockResolvedValue([
      source(),
      source({
        id: "source-2",
        name: "Other workspace source",
        workspace_id: "ws-other",
      }),
    ]);
    mockListMembers.mockResolvedValue([{ user_id: "user-1", role: "owner" }]);
    render(<SourcesPage />, { wrapper: Wrapper });
    // The shared list guard rejects the mismatched payload before caching;
    // the foreign source name is never rendered as a selectable option.
    expect(
      await screen.findByText(/failed to load work sources/i),
    ).toBeInTheDocument();
    expect(screen.queryByText(/other workspace source/i)).not.toBeInTheDocument();
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();
  });

  it("switches between same-workspace sources without auto-creating", async () => {
    const user = userEvent.setup();
    mockListWorkSources.mockResolvedValue([
      source(),
      source({ id: "source-3", name: "Second source" }),
      source({ id: "source-4", name: "Disabled", enabled: false }),
    ]);
    await renderPage();
    const select = screen.getByRole("combobox", { name: /source/i });
    expect((select as HTMLSelectElement).value).toBe("source-1");

    mockCreateWorkSourceCommand.mockClear();
    await user.selectOptions(select, "source-3");
    await waitFor(() =>
      expect((select as HTMLSelectElement).value).toBe("source-3"),
    );
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();

    await user.selectOptions(select, "source-4");
    await waitFor(() => expect((select as HTMLSelectElement).value).toBe("source-4"));
    expect(screen.getByRole("button", { name: /refresh/i })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: /existing results/i })).toBeEnabled();
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();

    await user.selectOptions(select, "source-1");
    await waitFor(() =>
      expect((select as HTMLSelectElement).value).toBe("source-1"),
    );
    expect(mockCreateWorkSourceCommand).not.toHaveBeenCalled();
  });
});
