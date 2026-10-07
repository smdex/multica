import type { ChatSession } from "@multica/core/types";

export type ChatThreadListFilter = {
  agentId: string | null;
  query: string;
};

// Keep the search text aligned with the compact row preview rather than
// matching markdown syntax a person cannot see in the list.
export function chatThreadPreview(content: string): string {
  return content
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/[#*`>~]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

export function filterChatThreadSessions(
  sessions: ChatSession[],
  { agentId, query }: ChatThreadListFilter,
): ChatSession[] {
  const search = query.trim().toLowerCase();

  return sessions.filter((session) => {
    if (agentId && session.agent_id !== agentId) return false;
    if (!search) return true;

    return [session.title, chatThreadPreview(session.last_message?.content ?? "")].some((value) =>
      value.toLowerCase().includes(search),
    );
  });
}
