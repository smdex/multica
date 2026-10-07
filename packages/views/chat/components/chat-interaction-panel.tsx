"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { useRespondToChatInteraction } from "@multica/core/chat/mutations";
import { workflowRequestOptions } from "@multica/core/chat/queries";
import type {
  ChatControls,
  ChatInteraction,
  ChatInteractionAnswer,
  ChatInteractionResponse,
  WorkflowCapabilities,
} from "@multica/core/types";
import { useT } from "../../i18n";

interface SubmittedInteraction {
  id: string;
  runtimeId: string;
}

function isUnsettled(interaction: ChatInteraction): boolean {
  return (
    interaction.status === "pending" ||
    interaction.status === "resolving" ||
    interaction.status === "unknown"
  );
}

function canRespond(
  interaction: ChatInteraction,
  controls: ChatControls | undefined,
  capabilities: WorkflowCapabilities | undefined,
  sessionId: string,
): boolean {
  // Only a server-pending prompt admits a new provider response. Resolving and
  // unknown deliveries are visible for recovery, never a second ambiguous
  // provider write.
  if (
    !controls ||
    !capabilities?.online ||
    interaction.kind === "unknown" ||
    interaction.status !== "pending"
  ) return false;
  if (
    controls.chat_session_id !== sessionId ||
    !controls.active ||
    controls.task_id !== interaction.task_id ||
    controls.run_id !== interaction.run_id ||
    controls.turn_id !== interaction.turn_id ||
    !controls.runtime_id
  ) {
    return false;
  }
  return interaction.kind === "approval"
    ? capabilities.controls.approvals && controls.can_approve
    : capabilities.controls.questions && controls.can_answer;
}

function RequestDetails({ interaction }: { interaction: ChatInteraction }) {
  const { t } = useT("chat");
  const entries = Object.entries(interaction.input);
  if (!interaction.description && !interaction.tool && entries.length === 0) return null;

  return (
    <details className="rounded-md border border-border bg-muted/30 px-2 py-1.5 text-caption">
      <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
        {t(($) => $.workflow.interaction.details)}
      </summary>
      <div className="mt-2 space-y-1 text-foreground">
        {interaction.description && <p className="whitespace-pre-wrap">{interaction.description}</p>}
        {interaction.tool && <p className="font-mono text-muted-foreground">{interaction.tool}</p>}
        {entries.length > 0 && (
          <dl className="space-y-1 break-words text-muted-foreground">
            {entries.map(([key, value]) => (
              <div key={key} className="grid grid-cols-[minmax(0,auto)_1fr] gap-x-2">
                <dt>{key}</dt>
                <dd>{typeof value === "string" ? value : JSON.stringify(value)}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>
    </details>
  );
}

function QuestionResponse({
  interaction,
  disabled,
  onRespond,
}: {
  interaction: ChatInteraction;
  disabled: boolean;
  onRespond: (response: ChatInteractionResponse) => void;
}) {
  const { t } = useT("chat");
  const [selected, setSelected] = useState<Record<string, string[]>>({});
  const [text, setText] = useState<Record<string, string>>({});
  const hasSecret = interaction.questions.some((question) => question.secret);
  const hasEveryAnswer = interaction.questions.every((question) => {
    if (question.secret) return false;
    return (selected[question.id]?.length ?? 0) > 0 ||
      (question.allow_text && Boolean(text[question.id]?.trim()));
  });
  const submit = () => {
    const answers: ChatInteractionAnswer[] = interaction.questions.map((question) => ({
      question_id: question.id,
      option_ids: selected[question.id] ?? [],
      text: question.allow_text && !question.secret ? (text[question.id] ?? "") : "",
    }));
    onRespond({ answers });
  };

  return (
    <div className="space-y-3">
      {interaction.questions.map((question) => (
        <fieldset key={question.id} className="space-y-2" disabled={disabled || question.secret}>
          <legend className="text-label font-medium">{question.prompt}</legend>
          {question.options.map((option) => {
            const isSelected = (selected[question.id] ?? []).includes(option.id);
            const optionId = `${interaction.id}-${question.id}-${option.id}`;
            if (!question.multiple) {
              return (
                <label key={option.id} htmlFor={optionId} className="flex cursor-pointer items-start gap-2">
                  <input
                    id={optionId}
                    type="radio"
                    name={`${interaction.id}-${question.id}`}
                    checked={isSelected}
                    onChange={() => {
                      setSelected((current) => ({ ...current, [question.id]: [option.id] }));
                      setText((current) => ({ ...current, [question.id]: "" }));
                    }}
                    className="mt-0.5"
                  />
                  <span>
                    <span className="block text-body">{option.label}</span>
                    {option.description && (
                      <span className="block text-caption text-muted-foreground">{option.description}</span>
                    )}
                  </span>
                </label>
              );
            }
            return (
              <label key={option.id} className="flex cursor-pointer items-start gap-2">
                <Checkbox
                  checked={isSelected}
                  onCheckedChange={(checked) => {
                    setSelected((current) => {
                      const currentIds = current[question.id] ?? [];
                      const nextIds = checked
                        ? [...currentIds, option.id]
                        : currentIds.filter((id) => id !== option.id);
                      return { ...current, [question.id]: nextIds };
                    });
                    setText((current) => ({ ...current, [question.id]: "" }));
                  }}
                />
                <span>
                  <span className="block text-body">{option.label}</span>
                  {option.description && (
                    <span className="block text-caption text-muted-foreground">{option.description}</span>
                  )}
                </span>
              </label>
            );
          })}
          {question.allow_text && !question.secret && (
            <div className="space-y-1">
              <label htmlFor={`${interaction.id}-${question.id}-text`} className="text-caption text-muted-foreground">
                {t(($) => $.workflow.interaction.optional_text)}
              </label>
              <Textarea
                id={`${interaction.id}-${question.id}-text`}
                value={text[question.id] ?? ""}
                onChange={(event) => {
                  setText((current) => ({ ...current, [question.id]: event.target.value }));
                  setSelected((current) => ({ ...current, [question.id]: [] }));
                }}
                className="min-h-20 text-body"
              />
            </div>
          )}
        </fieldset>
      ))}
      {hasSecret ? (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.workflow.interaction.secret_unsupported)}
        </p>
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" onClick={submit} disabled={disabled || !hasEveryAnswer}>
            {t(($) => $.workflow.interaction.submit)}
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={() => onRespond({ cancelled: true })} disabled={disabled}>
            {t(($) => $.workflow.interaction.cancel)}
          </Button>
        </div>
      )}
    </div>
  );
}

function InteractionCard({
  sessionId,
  interaction,
  controls,
  capabilities,
  disabled,
  onRespond,
}: {
  sessionId: string;
  interaction: ChatInteraction;
  controls: ChatControls | undefined;
  capabilities: WorkflowCapabilities | undefined;
  disabled: boolean;
  onRespond: (interaction: ChatInteraction, response: ChatInteractionResponse) => void;
}) {
  const { t } = useT("chat");
  const admitted = canRespond(interaction, controls, capabilities, sessionId);
  const unavailable = !admitted || interaction.status === "unknown" || interaction.kind === "unknown";
  const approvalContext = ["action", "command", "path"]
    .flatMap((key) => {
      const value = interaction.input[key];
      return typeof value === "string" && value ? [[key, value] as const] : [];
    });

  return (
    <section data-slot={`chat-interaction-${interaction.id}`} className="space-y-3 rounded-lg border border-border bg-surface-raised p-3">
      <div className="space-y-1">
        <h3 className="text-label font-medium">{interaction.title}</h3>
        {interaction.kind === "approval" && (interaction.tool || approvalContext.length > 0) && (
          <div className="space-y-0.5 text-caption text-muted-foreground">
            {interaction.tool && <p className="font-mono">{interaction.tool}</p>}
            {approvalContext.map(([key, value]) => <p key={key}>{key}: {value}</p>)}
          </div>
        )}
        {unavailable && (
          <p className="text-caption text-muted-foreground">
            {t(($) => $.workflow.interaction.unavailable)}
          </p>
        )}
      </div>
      <RequestDetails interaction={interaction} />
      {interaction.kind === "approval" ? (
        <div className="flex flex-wrap gap-2">
          <Button
            type="button"
            size="sm"
            disabled={disabled || unavailable}
            onClick={() => onRespond(interaction, { choice_id: "allow_once" })}
          >
            {t(($) => $.workflow.interaction.allow_once)}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || unavailable}
            onClick={() => onRespond(interaction, { choice_id: "deny" })}
          >
            {t(($) => $.workflow.interaction.deny)}
          </Button>
        </div>
      ) : interaction.kind === "question" ? (
        <QuestionResponse
          interaction={interaction}
          disabled={disabled || unavailable}
          onRespond={(response) => onRespond(interaction, response)}
        />
      ) : null}
    </section>
  );
}

export function ChatInteractionPanel({
  wsId,
  sessionId,
  controls,
  capabilities,
  interactions,
}: {
  wsId: string;
  sessionId: string;
  controls: ChatControls | undefined;
  capabilities: WorkflowCapabilities | undefined;
  interactions: ChatInteraction[];
}) {
  const { t } = useT("chat");
  const respond = useRespondToChatInteraction();
  const [submitted, setSubmitted] = useState<SubmittedInteraction | null>(null);
  const [responseFailed, setResponseFailed] = useState(false);
  const { data: operation } = useQuery(
    workflowRequestOptions(wsId, submitted?.runtimeId ?? "", submitted?.id ?? ""),
  );
  const visibleInteractions = interactions.filter(isUnsettled);

  useEffect(() => {
    setSubmitted(null);
    setResponseFailed(false);
  }, [sessionId]);

  if (visibleInteractions.length === 0) return null;

  const submitResponse = async (interaction: ChatInteraction, response: ChatInteractionResponse) => {
    if (!controls || !canRespond(interaction, controls, capabilities, sessionId) || respond.isPending) return;
    setResponseFailed(false);
    try {
      const request = await respond.mutateAsync({
        sessionId,
        interactionId: interaction.id,
        runtimeId: controls.runtime_id,
        taskId: interaction.task_id,
        runId: interaction.run_id,
        turnId: interaction.turn_id,
        response,
      });
      setSubmitted({ id: request.id, runtimeId: controls.runtime_id });
    } catch {
      setResponseFailed(true);
    }
  };

  const waiting = respond.isPending || operation?.status === "pending" || operation?.status === "running";
  const uncertain = operation?.status === "unknown" ||
    (operation?.result && "delivery" in operation.result && operation.result.delivery === "unknown");

  return (
    <section data-slot="chat-interaction-panel" aria-label={t(($) => $.workflow.mode_label)} className="mx-4 mb-2 space-y-2">
      {visibleInteractions.map((interaction) => (
        <InteractionCard
          key={interaction.id}
          sessionId={sessionId}
          interaction={interaction}
          controls={controls}
          capabilities={capabilities}
          disabled={waiting}
          onRespond={(item, response) => void submitResponse(item, response)}
        />
      ))}
      {waiting && <p className="text-caption text-muted-foreground">{t(($) => $.workflow.interaction.responding)}</p>}
      {(responseFailed || operation?.status === "failed") && (
        <p role="alert" className="text-caption text-destructive">{t(($) => $.workflow.request_failed)}</p>
      )}
      {uncertain && (
        <p role="status" className="text-caption text-muted-foreground">{t(($) => $.workflow.operation_unknown)}</p>
      )}
    </section>
  );
}
