"use client";

import { useT } from "../../i18n";

/** Server-authoritative policy selector for a first-party chat. */
export function ChatInteractionMode({
  mode,
  disabled,
  busy,
  onChange,
}: {
  mode: "chat" | "autonomous";
  disabled: boolean;
  busy: boolean;
  onChange: (mode: "chat" | "autonomous") => void;
}) {
  const { t } = useT("chat");

  return (
    <fieldset data-slot="chat-interaction-mode" className="mx-4 mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-caption" disabled={disabled || busy}>
      <legend className="sr-only">{t(($) => $.workflow.mode_label)}</legend>
      <span className="text-muted-foreground">{t(($) => $.workflow.mode_label)}</span>
      <label className="flex cursor-pointer items-center gap-1.5 disabled:cursor-not-allowed">
        <input
          type="radio"
          name="chat-interaction-mode"
          value="chat"
          checked={mode === "chat"}
          onChange={() => onChange("chat")}
        />
        {t(($) => $.workflow.mode_chat)}
      </label>
      <label className="flex cursor-pointer items-center gap-1.5 disabled:cursor-not-allowed">
        <input
          type="radio"
          name="chat-interaction-mode"
          value="autonomous"
          checked={mode === "autonomous"}
          onChange={() => onChange("autonomous")}
        />
        {t(($) => $.workflow.mode_autonomous)}
      </label>
      <span className="basis-full text-muted-foreground">{t(($) => $.workflow.mode_help)}</span>
    </fieldset>
  );
}
