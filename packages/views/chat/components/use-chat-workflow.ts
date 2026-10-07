"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import {
  chatControlsOptions,
  chatInteractionsOptions,
  workflowCapabilitiesOptions,
  workflowRequestOptions,
} from "@multica/core/chat/queries";
import {
  useSetChatSessionInteractionMode,
  useSteerChatSession,
} from "@multica/core/chat/mutations";
import type { Attachment, ChatSession, WorkflowRequest } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";

type CommitInput = (options?: { extraDraftKeys?: string[]; clearEditor?: boolean }) => void;

export type ChatWorkflowSend = (
  content: string,
  attachmentIds: string[] | undefined,
  commitInput: CommitInput,
  draftAttachments: Attachment[],
) => Promise<boolean>;

export type ChatWorkflowError =
  | "attachments_unsupported"
  | "delivery_rejected"
  | "delivery_unknown"
  | "request_failed";

interface SteerSubmission {
  key: string;
  id: string;
  wsId: string;
  runtimeId: string;
  sessionId: string;
  commitInput: CommitInput;
}

type SteerSubmissions = Record<string, SteerSubmission>;
type SteerErrors = Record<string, ChatWorkflowError>;
type WorkflowRequestQuery = Parameters<
  Exclude<
    ReturnType<typeof workflowRequestOptions>["refetchInterval"],
    number | false | undefined
  >
>[0];

function steerSessionKey(wsId: string, sessionId: string): string {
  return `${wsId}\u0000${sessionId}`;
}

function steerSubmissionKey(wsId: string, sessionId: string, requestId: string): string {
  return `${steerSessionKey(wsId, sessionId)}\u0000${requestId}`;
}

function isAcceptedDelivery(operation: WorkflowRequest | undefined): boolean {
  return Boolean(
    operation?.status === "completed" &&
      operation.result &&
      "delivery" in operation.result &&
      operation.result.delivery === "accepted",
  );
}

function hasUnknownDelivery(operation: WorkflowRequest | undefined): boolean {
  return Boolean(
    operation?.status === "unknown" ||
      (operation?.status === "completed" &&
        operation.result &&
        "delivery" in operation.result &&
        operation.result.delivery === "unknown"),
  );
}

/**
 * The shared live-control boundary for the page and floating chat window.
 * It trusts only the server's current capability and exact task/run/turn
 * admission record; provider names and a pending chat task never enable a
 * control by themselves.
 */
export function useChatWorkflow({
  wsId,
  session,
  runtimeId,
  visible,
}: {
  wsId: string;
  session: ChatSession | null;
  runtimeId: string | undefined;
  visible: boolean;
}) {
  const sessionId = session?.id ?? "";
  const resolvedRuntimeId = runtimeId ?? "";
  const { data: capabilities } = useQuery(
    workflowCapabilitiesOptions(wsId, resolvedRuntimeId),
  );
  const { data: controls } = useQuery(
    chatControlsOptions(wsId, sessionId, visible),
  );
  const { data: interactions } = useQuery(
    chatInteractionsOptions(wsId, sessionId, visible),
  );
  const setMode = useSetChatSessionInteractionMode();
  const steer = useSteerChatSession();
  const steerSubmissionsRef = useRef<SteerSubmissions>({});
  const steerInFlightSessionKeysRef = useRef(new Set<string>());
  const [steerSubmissions, setSteerSubmissions] = useState<SteerSubmissions>({});
  const [steerErrors, setSteerErrors] = useState<SteerErrors>({});
  const [modeErrorSessionId, setModeErrorSessionId] = useState<string | null>(null);

  const registerSteerSubmission = useCallback((submission: SteerSubmission) => {
    const next = { ...steerSubmissionsRef.current, [submission.key]: submission };
    steerSubmissionsRef.current = next;
    setSteerSubmissions(next);
  }, []);

  const removeSteerSubmission = useCallback((key: string): SteerSubmission | null => {
    const submission = steerSubmissionsRef.current[key];
    if (!submission) return null;
    const { [key]: _removed, ...next } = steerSubmissionsRef.current;
    steerSubmissionsRef.current = next;
    setSteerSubmissions(next);
    return submission;
  }, []);

  const setSteerError = useCallback((workspaceId: string, chatSessionId: string, value: ChatWorkflowError | null) => {
    const key = steerSessionKey(workspaceId, chatSessionId);
    setSteerErrors((previous) => {
      if (value === null) {
        if (!(key in previous)) return previous;
        const { [key]: _cleared, ...next } = previous;
        return next;
      }
      if (previous[key] === value) return previous;
      return { ...previous, [key]: value };
    });
  }, []);

  // Keep every unresolved request, including those for a chat the user has
  // navigated away from. A transport failure has no trustworthy POST result,
  // so polling its original identity is the only safe reconciliation; posting
  // the provider input again would create a second steering command.
  const workspaceSteerSubmissions = Object.values(steerSubmissions)
    .filter((submission) => submission.wsId === wsId);
  const steerOperationQueries = useQueries({
    queries: workspaceSteerSubmissions.map((submission) => ({
      ...workflowRequestOptions(submission.wsId, submission.runtimeId, submission.id),
      // `workflowRequestOptions` stops after a query error because it has no
      // status to inspect. This request may exist despite a lost POST response,
      // so keep reconciling this exact id until the server gives a result.
      refetchInterval: (query: WorkflowRequestQuery) => {
        const operation = query.state.data as WorkflowRequest | undefined;
        return operation?.status === "pending" || operation?.status === "running" || !operation
          ? 1_000
          : false;
      },
    })),
  });
  const currentSteerSubmission = workspaceSteerSubmissions.find(
    (submission) => submission.sessionId === sessionId,
  );
  const currentSteerOperationIndex = currentSteerSubmission
    ? workspaceSteerSubmissions.indexOf(currentSteerSubmission)
    : -1;
  const steerOperation = currentSteerOperationIndex >= 0
    ? steerOperationQueries[currentSteerOperationIndex]?.data
    : undefined;

  const reconcileSteerSubmission = useCallback((submission: SteerSubmission, operation: WorkflowRequest) => {
    if (isAcceptedDelivery(operation)) {
      const settled = removeSteerSubmission(submission.key);
      if (settled) settled.commitInput();
      return;
    }

    if (operation.status === "failed") {
      if (removeSteerSubmission(submission.key)) {
        setSteerError(submission.wsId, submission.sessionId, "request_failed");
      }
      return;
    }

    if (
      operation.status === "completed" &&
      operation.result &&
      "delivery" in operation.result &&
      operation.result.delivery === "rejected"
    ) {
      if (removeSteerSubmission(submission.key)) {
        setSteerError(submission.wsId, submission.sessionId, "delivery_rejected");
      }
      return;
    }

    if (hasUnknownDelivery(operation)) {
      // Unknown is deliberately not removed. It remains keyed by the original
      // request so another Send now cannot issue a duplicate provider input.
      setSteerError(submission.wsId, submission.sessionId, "delivery_unknown");
    }
  }, [removeSteerSubmission, setSteerError]);

  useEffect(() => {
    workspaceSteerSubmissions.forEach((submission, index) => {
      const operation = steerOperationQueries[index]?.data;
      if (operation) reconcileSteerSubmission(submission, operation);
    });
  }, [reconcileSteerSubmission, steerOperationQueries, workspaceSteerSubmissions]);

  const hasInteractiveCapability = Boolean(
    capabilities?.online &&
      (capabilities.controls.steer || capabilities.controls.approvals || capabilities.controls.questions),
  );
  // Unknown is explicitly unsettled in the public contract. It must keep the
  // appropriate controls unavailable until the server gives a conclusive view.
  const hasUnsettledInteraction = (interactions?.items ?? []).some(
    (interaction) =>
      interaction.status === "pending" ||
      interaction.status === "resolving" ||
      interaction.status === "unknown" ||
      interaction.kind === "unknown",
  );
  const operationBlocksSteer =
    Boolean(currentSteerSubmission) &&
    (!steerOperation ||
      steerOperation.status === "pending" ||
      steerOperation.status === "running" ||
      hasUnknownDelivery(steerOperation));
  const hasExactSteerAdmission = Boolean(
    controls?.chat_session_id === sessionId &&
      controls.runtime_id === resolvedRuntimeId &&
      controls.active &&
      controls.can_steer &&
      controls.task_id &&
      controls.run_id &&
      controls.turn_id,
  );
  const canSendNow = hasInteractiveCapability && hasExactSteerAdmission &&
    !hasUnsettledInteraction && !operationBlocksSteer;
  const steerError = steerErrors[steerSessionKey(wsId, sessionId)] ?? null;

  const handleSendNow: ChatWorkflowSend = async (
    content,
    attachmentIds,
    commitInput,
    draftAttachments,
  ) => {
    // The steering endpoint only accepts content. Refuse rather than turning a
    // Send now click into a partial message with silently dropped attachments.
    if (attachmentIds?.length || draftAttachments.length) {
      setSteerError(wsId, sessionId, "attachments_unsupported");
      return false;
    }
    const currentSessionKey = steerSessionKey(wsId, sessionId);
    if (
      !sessionId ||
      !controls ||
      !canSendNow ||
      steerInFlightSessionKeysRef.current.has(currentSessionKey)
    ) {
      return false;
    }

    const requestId = createSafeId();
    const submission = {
      key: steerSubmissionKey(wsId, sessionId, requestId),
      id: requestId,
      wsId,
      runtimeId: controls.runtime_id,
      sessionId,
      commitInput,
    };
    // Register before POST. If the network loses the acknowledgement after the
    // server commits, the query above still knows the only id it may reconcile.
    registerSteerSubmission(submission);
    steerInFlightSessionKeysRef.current.add(currentSessionKey);
    setSteerError(wsId, sessionId, null);
    try {
      const request = await steer.mutateAsync({
        sessionId,
        runtimeId: controls.runtime_id,
        requestId,
        taskId: controls.task_id!,
        runId: controls.run_id!,
        turnId: controls.turn_id!,
        content,
      });

      // A completed acknowledgement may be returned by a replay. Let the
      // composer own this synchronous commit, then remove the registered
      // operation so polling cannot commit the same draft again.
      if (isAcceptedDelivery(request)) {
        removeSteerSubmission(submission.key);
        return true;
      }

      reconcileSteerSubmission(submission, request);
      // Pending and uncertain requests retain the draft. They never become a
      // queue send or an implicit retry.
      return false;
    } catch {
      // Do not discard the preallocated id here: a transport error cannot tell
      // whether the server already accepted the exact task/run/turn input.
      setSteerError(wsId, sessionId, "delivery_unknown");
      return false;
    } finally {
      steerInFlightSessionKeysRef.current.delete(currentSessionKey);
    }
  };

  const setInteractionMode = async (interactionMode: "chat" | "autonomous") => {
    if (!sessionId || !hasInteractiveCapability) return false;
    setModeErrorSessionId(null);
    try {
      await setMode.mutateAsync({ sessionId, interactionMode });
      return true;
    } catch {
      setModeErrorSessionId(sessionId);
      return false;
    }
  };

  return {
    capabilities,
    controls,
    interactions: interactions?.items ?? [],
    interactionMode: session?.interaction_mode ?? "autonomous",
    canUseChatMode: hasInteractiveCapability,
    canSendNow,
    sendNowUnavailable: hasUnsettledInteraction || operationBlocksSteer || !hasExactSteerAdmission,
    handleSendNow,
    steerOperation,
    steerError,
    modeError: modeErrorSessionId === sessionId,
    setInteractionMode,
    isModeUpdating: setMode.isPending,
  };
}
