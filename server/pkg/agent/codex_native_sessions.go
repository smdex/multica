package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const codexNativePreparationTimeout = 20 * time.Second
const codexNativeMaxResponseBytes = 24 << 20

func (b *codexBackend) withCodexNativeClient(ctx context.Context, fn func(context.Context, *codexClient) error) error {
	execName := b.cfg.ExecutablePath
	if execName == "" {
		execName = "codex"
	}
	if _, err := exec.LookPath(execName); err != nil {
		return nativeSessionError(NativeSessionUnsupported, "Codex executable not found at %q: %v", execName, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, codexNativePreparationTimeout)
	defer cancel()
	cmd := b.cfg.commandAt(execName).exec(runCtx, "app-server")
	cmd.Env = buildEnv(b.cfg.Env)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nativeSessionError(NativeSessionResumeUnavailable, "open Codex app-server stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nativeSessionError(NativeSessionResumeUnavailable, "open Codex app-server stdout: %v", err)
	}
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nativeSessionError(NativeSessionResumeUnavailable, "start Codex app-server: %v", err)
	}
	client := &codexClient{stdin: stdin, pending: make(map[int]*pendingRPC)}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		// Codex serializes a hydrated thread into one app-server JSON-RPC
		// response. Its limit is the native-history report limit, not the 1 MiB
		// JSONL-record limit used for Pi/Claude file parsing.
		scanner.Buffer(make([]byte, 4096), codexNativeMaxResponseBytes)
		for scanner.Scan() {
			var envelope map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
				continue
			}
			// Read-only history must never dispatch app-server requests or
			// notifications through execution's approval/event handlers.
			if _, ok := envelope["id"]; ok && envelope["method"] == nil && (envelope["result"] != nil || envelope["error"] != nil) {
				client.handleResponse(envelope)
			}
		}
		scanErr := scanner.Err()
		if scanErr == bufio.ErrTooLong || (scanErr != nil && strings.Contains(scanErr.Error(), "token too long")) {
			scanErr = nativeSessionError(NativeSessionHistoryTooLarge, "Codex app-server response exceeds %d bytes", codexNativeMaxResponseBytes)
		}
		client.markProcessExited(scanErr)
	}()
	defer func() {
		cancel()
		_ = stdin.Close()
		signalProcessGroup(cmd, syscall.SIGKILL)
		_ = stdout.Close()
		_ = cmd.Wait()
		<-readerDone
		releaseProcessGroup(cmd)
	}()
	if _, err := client.request(runCtx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "multica-agent-sdk", "title": "Multica Agent SDK", "version": "0.2.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return nativeSessionError(NativeSessionResumeUnavailable, "initialize Codex app-server: %v", err)
	}
	client.notify("initialized")
	return fn(runCtx, client)
}

func codexNativeString(value any) string {
	stringValue, _ := value.(string)
	return strings.TrimSpace(stringValue)
}

func codexNativeMap(value any) map[string]any {
	mapValue, _ := value.(map[string]any)
	return mapValue
}

func codexNativeRevision(thread map[string]any) (string, error) {
	id := codexNativeString(thread["id"])
	updated, exists := thread["updatedAt"]
	if !exists {
		updated, exists = thread["createdAt"]
	}
	if id == "" || !exists {
		return "", nativeSessionError(NativeSessionInvalidHistory, "Codex thread lacks id or revision metadata")
	}
	encoded, err := json.Marshal([]any{id, updated})
	if err != nil {
		return "", nativeSessionError(NativeSessionInvalidHistory, "encode Codex thread revision: %v", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func codexNativeTime(value any) time.Time {
	switch value := value.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err == nil {
			return parsed.UTC()
		}
	case float64:
		return time.Unix(int64(value), int64((value-float64(int64(value)))*1e9)).UTC()
	}
	return time.Time{}
}

func codexThreadSummary(thread map[string]any, revision string) (NativeSessionSummary, error) {
	id := codexNativeString(thread["id"])
	if id == "" {
		return NativeSessionSummary{}, nativeSessionError(NativeSessionInvalidHistory, "Codex thread lacks id")
	}
	cwd := codexNativeString(thread["cwd"])
	preview := codexNativeString(thread["preview"])
	title := codexNativeString(thread["name"])
	if title == "" {
		title = nativeSessionPreview(preview)
	}
	if title == "" {
		title = nativeSessionTitle("Codex", cwd, id)
	}
	updated := codexNativeTime(thread["updatedAt"])
	if updated.IsZero() {
		updated = codexNativeTime(thread["createdAt"])
	}
	return NativeSessionSummary{NativeID: id, Handle: id, Revision: revision, Title: title, Cwd: cwd, Preview: nativeSessionPreview(preview), UpdatedAt: updated, Model: codexNativeString(thread["model"])}, nil
}

func (b *codexBackend) ListNativeSessions(ctx context.Context, opts NativeSessionListOptions) (NativeSessionPage, error) {
	limit := nativeSessionLimit(opts.Limit)
	var page NativeSessionPage
	err := b.withCodexNativeClient(ctx, func(runCtx context.Context, client *codexClient) error {
		params := map[string]any{"limit": limit, "modelProviders": []any{}}
		if opts.Cursor != "" {
			params["cursor"] = opts.Cursor
		}
		raw, err := client.request(runCtx, "thread/list", params)
		if err != nil {
			if nativeSessionErrorCode(err) != "" {
				return err
			}
			return nativeSessionError(NativeSessionResumeUnavailable, "list Codex threads: %v", err)
		}
		var response struct {
			Data       []map[string]any `json:"data"`
			NextCursor string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &response); err != nil || response.Data == nil || len(response.Data) > nativeSessionMaxFiles {
			return nativeSessionError(NativeSessionInvalidHistory, "invalid Codex thread/list response")
		}
		page.NextCursor = response.NextCursor
		page.Truncated = len(response.Data) >= nativeSessionMaxFiles
		for _, thread := range response.Data {
			revision, err := codexNativeRevision(thread)
			if err != nil {
				continue
			}
			summary, err := codexThreadSummary(thread, revision)
			if err == nil {
				page.Sessions = append(page.Sessions, summary)
			}
		}
		return nil
	})
	return page, err
}

func codexThreadFromRead(raw json.RawMessage) (map[string]any, error) {
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "decode Codex thread/read response")
	}
	thread := codexNativeMap(response["thread"])
	if thread == nil {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "Codex thread/read lacks thread")
	}
	return thread, nil
}

func codexItemText(value any) string {
	text, _, _ := nativeMessageContent(value)
	return text
}

const codexNativeMissingTimestampWarning = "Codex history omitted native timestamps; used a deterministic import-time sequence"

func codexNativeSnapshot(thread map[string]any) (NativeSessionSnapshot, error) {
	return codexNativeSnapshotAt(thread, time.Now().UTC())
}

func codexNativeSnapshotAt(thread map[string]any, importedAt time.Time) (NativeSessionSnapshot, error) {
	revision, err := codexNativeRevision(thread)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	summary, err := codexThreadSummary(thread, revision)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	if _, err := nativeWorkdir(summary.Cwd); err != nil {
		return NativeSessionSnapshot{}, err
	}
	turns, _ := thread["turns"].([]any)
	if turns == nil {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionInvalidHistory, "Codex thread/read omitted turns")
	}
	var messages []NativeHistoryMessage
	var warnings []string
	for _, rawTurn := range turns {
		turn := codexNativeMap(rawTurn)
		if turn == nil {
			return NativeSessionSnapshot{}, nativeSessionError(NativeSessionInvalidHistory, "Codex thread has an invalid turn")
		}
		items, _ := turn["items"].([]any)
		if items == nil {
			return NativeSessionSnapshot{}, nativeSessionError(NativeSessionInvalidHistory, "Codex thread has invalid turn items")
		}
		// Imported assistant rows render their settled events before Content. Keep
		// source-order activity until the next assistant text boundary so a tool
		// completed before that text remains before it in the rendered timeline.
		var pending []Message
		for _, rawItem := range items {
			item := codexNativeMap(rawItem)
			if item == nil {
				return NativeSessionSnapshot{}, nativeSessionError(NativeSessionInvalidHistory, "Codex thread has invalid item")
			}
			kind := strings.ToLower(codexNativeString(item["type"]))
			id := codexNativeString(item["id"])
			switch kind {
			case "usermessage", "user_message":
				created, err := codexNativeItemTime(item, turn)
				if err != nil {
					return NativeSessionSnapshot{}, err
				}
				codexNativeAttachPending(messages, &pending, &warnings)
				messages = append(messages, NativeHistoryMessage{NativeID: id, Role: "user", Content: codexItemText(item["content"]), CreatedAt: created})
			case "agentmessage", "agent_message":
				created, err := codexNativeItemTime(item, turn)
				if err != nil {
					return NativeSessionSnapshot{}, err
				}
				messages = append(messages, NativeHistoryMessage{NativeID: id, Role: "assistant", Content: codexNativeString(item["text"]), CreatedAt: created, Events: pending})
				pending = nil
			case "reasoning":
				text := codexItemText(item["summary"])
				if text == "" {
					text = codexItemText(item["content"])
				}
				if text != "" {
					pending = append(pending, Message{Type: MessageThinking, Content: text})
				}
			case "commandexecution", "command_execution", "filechange", "file_change", "mcptoolcall", "mcp_tool_call":
				events, settled := codexNativeToolEvents(kind, id, item)
				if !settled {
					warnings = append(warnings, "omitted unfinished historical Codex tool item")
					continue
				}
				pending = append(pending, events...)
			}
			if len(messages) > 10000 {
				return NativeSessionSnapshot{}, nativeSessionError(NativeSessionHistoryTooLarge, "Codex thread exceeds 10000 messages")
			}
		}
		codexNativeAttachPending(messages, &pending, &warnings)
	}
	if err := codexNativeAssignImportTimestamps(messages, importedAt, &warnings); err != nil {
		return NativeSessionSnapshot{}, err
	}
	events := 0
	for _, message := range messages {
		events += len(message.Events)
	}
	if events > 50000 {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionHistoryTooLarge, "Codex thread exceeds 50000 events")
	}
	return NativeSessionSnapshot{Summary: summary, Messages: messages, Warnings: sortedUniqueWarnings(warnings)}, nil
}

func codexNativeItemTime(item, turn map[string]any) (time.Time, error) {
	for _, source := range []struct {
		item map[string]any
		key  string
	}{
		{item: item, key: "timestamp"},
		{item: turn, key: "createdAt"},
	} {
		value, exists := source.item[source.key]
		if !exists || value == nil {
			continue
		}
		if stringValue, ok := value.(string); ok && strings.TrimSpace(stringValue) == "" {
			continue
		}
		parsed := codexNativeTime(value)
		if parsed.IsZero() {
			return time.Time{}, nativeSessionError(NativeSessionInvalidHistory, "Codex history has an invalid timestamp")
		}
		return parsed, nil
	}
	return time.Time{}, nil
}

func codexNativeAttachPending(messages []NativeHistoryMessage, pending *[]Message, warnings *[]string) {
	if len(*pending) == 0 {
		return
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "assistant" {
			messages[index].Events = append(messages[index].Events, *pending...)
			*pending = nil
			return
		}
	}
	*warnings = append(*warnings, "omitted historical Codex activity without an assistant message")
	*pending = nil
}

func codexNativeAssignImportTimestamps(messages []NativeHistoryMessage, importedAt time.Time, warnings *[]string) error {
	sequence := importedAt.UTC().Round(0)
	if sequence.IsZero() {
		return nativeSessionError(NativeSessionInvalidHistory, "Codex import timestamp is invalid")
	}
	missing := false
	for index := range messages {
		if messages[index].CreatedAt.IsZero() {
			messages[index].CreatedAt = sequence
			sequence = sequence.Add(time.Nanosecond)
			missing = true
		}
		messages[index].CreatedAt = messages[index].CreatedAt.UTC()
		if index > 0 && messages[index].CreatedAt.Before(messages[index-1].CreatedAt) {
			return nativeSessionError(NativeSessionInvalidHistory, "Codex history timestamps are out of source order")
		}
	}
	if missing {
		*warnings = append(*warnings, codexNativeMissingTimestampWarning)
	}
	return nil
}

func codexNativeToolEvents(kind, id string, item map[string]any) ([]Message, bool) {
	status := codexNormalizePatchStatus(codexNativeString(item["status"]))
	if status == "in_progress" {
		return nil, false
	}
	switch kind {
	case "commandexecution", "command_execution":
		resultStatus := status
		if resultStatus == "" && (extractNestedString(item, "error", "message") != "" || codexInt64(item, "exitCode", "exit_code") != 0) {
			resultStatus = "failed"
		}
		return []Message{
			{Type: MessageToolUse, Tool: "exec_command", CallID: id, Input: map[string]any{"command": codexNativeString(item["command"])}},
			{Type: MessageToolResult, Tool: "exec_command", CallID: id, Output: codexNativeCommandResultOutput(item, resultStatus), Status: resultStatus},
		}, true
	case "filechange", "file_change":
		changes := codexNormalizeRawChanges(item["changes"])
		output := codexPatchResultOutput(status, changes, codexNativeRawString(item["stdout"]), codexNativeRawString(item["stderr"]))
		output = codexNativeAppendFailure(output, status, item)
		return []Message{
			{Type: MessageToolUse, Tool: "patch_apply", CallID: id, Input: codexPatchInput(changes)},
			{Type: MessageToolResult, Tool: "patch_apply", CallID: id, Output: output, Status: status},
		}, true
	case "mcptoolcall", "mcp_tool_call":
		resultStatus := status
		if resultStatus == "" {
			resultStatus = "completed"
		}
		return []Message{
			{Type: MessageToolUse, Tool: codexMCPToolName(item), CallID: id, Input: codexMCPToolInput(item)},
			{Type: MessageToolResult, Tool: codexMCPToolName(item), CallID: id, Output: codexMCPToolResultOutput(item), Status: resultStatus},
		}, true
	default:
		return nil, false
	}
}

func codexNativeCommandResultOutput(item map[string]any, status string) string {
	output := codexNativeRawString(item["aggregatedOutput"])
	errorText := strings.TrimSpace(sanitizeCodexDiagnostic(extractNestedString(item, "error", "message")))
	exitCode := codexInt64(item, "exitCode", "exit_code")
	if status == "" && (errorText != "" || exitCode != 0) {
		status = "failed"
	}
	if status == "" || (status == "completed" && errorText == "" && exitCode == 0) {
		return output
	}
	parts := []string{status}
	if output != "" {
		parts = append(parts, output)
	}
	if exitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit code: %d", exitCode))
	}
	if errorText != "" {
		parts = append(parts, "error: "+errorText)
	}
	return strings.Join(parts, "\n")
}

func codexNativeAppendFailure(output, status string, item map[string]any) string {
	errorText := strings.TrimSpace(sanitizeCodexDiagnostic(extractNestedString(item, "error", "message")))
	if errorText == "" {
		return output
	}
	if output == "" {
		if status == "" {
			status = "failed"
		}
		output = status
	}
	return output + "\nerror: " + errorText
}

func codexNativeRawString(value any) string {
	stringValue, _ := value.(string)
	return stringValue
}

func codexReadNative(client *codexClient, ctx context.Context, threadID, expectedRevision string) (NativeSessionSnapshot, error) {
	return codexReadNativeAt(client, ctx, threadID, expectedRevision, time.Now().UTC())
}

func codexReadNativeAt(client *codexClient, ctx context.Context, threadID, expectedRevision string, importedAt time.Time) (NativeSessionSnapshot, error) {
	if strings.TrimSpace(threadID) == "" {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionNotFound, "empty Codex thread id")
	}
	raw, err := client.request(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	if err != nil {
		if nativeSessionErrorCode(err) != "" {
			return NativeSessionSnapshot{}, err
		}
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionNotFound, "read Codex thread: %v", err)
	}
	thread, err := codexThreadFromRead(raw)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	if codexNativeString(thread["id"]) != threadID {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionInvalidHistory, "Codex thread/read returned a different thread")
	}
	snapshot, err := codexNativeSnapshotAt(thread, importedAt)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	if expectedRevision != "" && snapshot.Summary.Revision != expectedRevision {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionSourceChanged, "Codex thread revision changed")
	}
	return snapshot, nil
}

func (b *codexBackend) ReadNativeSession(ctx context.Context, opts NativeSessionReadOptions) (NativeSessionSnapshot, error) {
	var snapshot NativeSessionSnapshot
	err := b.withCodexNativeClient(ctx, func(runCtx context.Context, client *codexClient) error {
		result, err := codexReadNative(client, runCtx, opts.Handle, opts.Revision)
		snapshot = result
		return err
	})
	return snapshot, err
}

func (b *codexBackend) PrepareNativeSession(ctx context.Context, opts NativeSessionPrepareOptions) (PreparedNativeSession, error) {
	manifestPath, stagePath, err := nativePreparationPaths(opts, "codex")
	if err != nil {
		return PreparedNativeSession{}, err
	}
	var prepared PreparedNativeSession
	err = withNativePreparationLock(manifestPath, func() error {
		manifest, exists, err := readNativePreparationManifest(manifestPath)
		if err != nil {
			return err
		}
		if exists {
			if !nativeManifestMatches(manifest, opts, "codex") {
				return nativeSessionError(NativeSessionSourceChanged, "native preparation id belongs to a different source")
			}
			result, err := b.ReadNativeSession(ctx, NativeSessionReadOptions{Handle: manifest.OwnedHandle, Revision: manifest.OwnedRevision})
			if err != nil {
				return err
			}
			result.ResumeSessionID = manifest.ResumeSession
			prepared = PreparedNativeSession{Snapshot: result, ResumeSessionID: manifest.ResumeSession, ResumeCwd: manifest.ResumeCwd}
			return nil
		}
		intentPath := nativePreparationIntentPath(stagePath)
		intent, hasIntent, err := readNativePreparationIntent(intentPath)
		if err != nil {
			return err
		}
		if hasIntent {
			if !nativeIntentMatches(intent, opts, "codex") {
				return nativeSessionError(NativeSessionSourceChanged, "native preparation intent belongs to a different source")
			}
			if intent.Phase != "owned" {
				return nativeSessionError(NativeSessionResumeUnavailable, "Codex native preparation has an uncertain fork; refusing another fork")
			}
			if intent.OwnedHandle == "" || intent.OwnedHandle == opts.Handle || intent.OwnedRevision == "" || intent.ResumeSession != intent.OwnedHandle || intent.ResumeCwd == "" {
				return nativeSessionError(NativeSessionResumeUnavailable, "invalid owned Codex preparation intent")
			}
			owned, err := b.ReadNativeSession(ctx, NativeSessionReadOptions{Handle: intent.OwnedHandle, Revision: intent.OwnedRevision})
			if err != nil {
				return err
			}
			if owned.Summary.Cwd != intent.ResumeCwd {
				return nativeSessionError(NativeSessionSourceChanged, "owned Codex working directory changed")
			}
			manifest = nativePreparationManifest{ImportID: opts.ImportID, Provider: "codex", SourceHandle: opts.Handle, SourceRevision: opts.Revision, OwnedHandle: intent.OwnedHandle, OwnedRevision: intent.OwnedRevision, ResumeSession: intent.ResumeSession, ResumeCwd: intent.ResumeCwd}
			if err := writeNativePreparationManifest(manifestPath, manifest); err != nil {
				return err
			}
			owned.ResumeSessionID = intent.ResumeSession
			prepared = PreparedNativeSession{Snapshot: owned, ResumeSessionID: intent.ResumeSession, ResumeCwd: intent.ResumeCwd}
			return nil
		}
		return b.withCodexNativeClient(ctx, func(runCtx context.Context, client *codexClient) error {
			importedAt := time.Now().UTC()
			source, err := codexReadNativeAt(client, runCtx, opts.Handle, opts.Revision, importedAt)
			if err != nil {
				return err
			}
			// Persist admission before the native side effect. An uncertain result
			// must survive restart without granting permission to fork again.
			intent = nativePreparationIntent{ImportID: opts.ImportID, Provider: "codex", SourceHandle: opts.Handle, SourceRevision: opts.Revision, Phase: "cloning"}
			if err := writeNativeAtomicJSON(intentPath, intent); err != nil {
				return err
			}
			forkRaw, err := client.request(runCtx, "thread/fork", map[string]any{"threadId": opts.Handle, "ephemeral": false, "excludeTurns": false})
			if err != nil {
				return nativeSessionError(NativeSessionResumeUnavailable, "Codex fork outcome is uncertain: %v", err)
			}
			forkThread, err := codexThreadFromRead(forkRaw)
			if err != nil {
				return nativeSessionError(NativeSessionResumeUnavailable, "invalid Codex fork response")
			}
			ownedID := codexNativeString(forkThread["id"])
			if ownedID == "" || ownedID == opts.Handle {
				return nativeSessionError(NativeSessionResumeUnavailable, "Codex fork did not create a distinct thread")
			}
			if _, err := codexReadNativeAt(client, runCtx, opts.Handle, opts.Revision, importedAt); err != nil {
				return err
			}
			owned, err := codexReadNativeAt(client, runCtx, ownedID, "", importedAt)
			if err != nil {
				return err
			}
			if !equivalentNativeHistory(source, owned) {
				return nativeSessionError(NativeSessionResumeUnavailable, "Codex fork does not preserve selected native history")
			}
			owned.ResumeSessionID = ownedID
			intent.Phase = "owned"
			intent.OwnedHandle = ownedID
			intent.OwnedRevision = owned.Summary.Revision
			intent.ResumeSession = ownedID
			intent.ResumeCwd = owned.Summary.Cwd
			if err := writeNativeAtomicJSON(intentPath, intent); err != nil {
				return err
			}
			manifest = nativePreparationManifest{ImportID: opts.ImportID, Provider: "codex", SourceHandle: opts.Handle, SourceRevision: opts.Revision, OwnedHandle: ownedID, OwnedRevision: owned.Summary.Revision, ResumeSession: ownedID, ResumeCwd: owned.Summary.Cwd}
			if err := writeNativePreparationManifest(manifestPath, manifest); err != nil {
				return err
			}
			prepared = PreparedNativeSession{Snapshot: owned, ResumeSessionID: ownedID, ResumeCwd: owned.Summary.Cwd}
			return nil
		})
	})
	return prepared, err
}
