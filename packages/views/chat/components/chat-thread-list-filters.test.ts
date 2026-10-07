// @vitest-environment node

import { describe, expect, it } from "vitest";
import type { ChatSession } from "@multica/core/types";
import {
  chatThreadPreview,
  filterChatThreadSessions,
} from "./chat-thread-list-filters";

function makeSession(
  id: string,
  {
    agentId = "agent-1",
    title = id,
    preview,
  }: { agentId?: string; title?: string; preview?: string } = {},
): ChatSession {
  return {
    id,
    workspace_id: "workspace-1",
    agent_id: agentId,
    creator_id: "user-1",
    title,
    status: "active",
    has_unread: false,
    created_at: "2026-09-19T00:00:00Z",
    updated_at: "2026-09-19T00:00:00Z",
    last_message: preview
      ? {
          content: preview,
          role: "assistant",
          created_at: "2026-09-19T00:00:00Z",
        }
      : null,
  };
}

describe("chatThreadPreview", () => {
  it("uses the same plain one-line text as the thread row", () => {
    expect(chatThreadPreview("# Ship **the**\n`checkpoint`")).toBe(
      "Ship the checkpoint",
    );
  });
});

describe("filterChatThreadSessions", () => {
  const sessions = [
    makeSession("design", { title: "Design review" }),
    makeSession("release", {
      agentId: "agent-2",
      title: "Release notes",
      preview: "# Ship **the**\n`checkpoint`",
    }),
    makeSession("research", { agentId: "agent-2", title: "Research" }),
  ];

  it("matches normalized title and visible preview text without changing order", () => {
    expect(
      filterChatThreadSessions(sessions, { agentId: null, query: "  DESIGN  " }).map(
        (session) => session.id,
      ),
    ).toEqual(["design"]);
    expect(
      filterChatThreadSessions(sessions, { agentId: null, query: "ship the checkpoint" }).map(
        (session) => session.id,
      ),
    ).toEqual(["release"]);
  });

  it("filters on the exact Multica agent ID, independently of provider metadata", () => {
    expect(
      filterChatThreadSessions(sessions, { agentId: "agent-2", query: "" }).map(
        (session) => session.id,
      ),
    ).toEqual(["release", "research"]);
  });
});
