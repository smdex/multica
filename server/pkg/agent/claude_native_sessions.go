//go:build linux || darwin

package agent

import (
	"context"
	"path/filepath"
	"strings"
)

type claudeNativeHeader struct {
	id      string
	cwd     string
	preview string
	model   string
}

func (b *claudeBackend) claudeNativeRoots() ([]string, error) {
	rootPath := strings.TrimSpace(effectiveNativeEnv(b.cfg, "CLAUDE_CONFIG_DIR"))
	if rootPath == "" {
		home, err := effectiveNativeHome(b.cfg)
		if err != nil {
			return nil, err
		}
		rootPath = filepath.Join(home, ".claude")
	}
	root, err := canonicalNativeRoot(filepath.Join(rootPath, "projects"))
	if err != nil {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "canonicalize Claude session root: %v", err)
	}
	if root == "" {
		return nil, nil
	}
	return []string{root}, nil
}

func parseClaudeNativeHeader(data []byte) (claudeNativeHeader, error) {
	rows, err := parseNativeHeaderJSONLRecords(data)
	if err != nil {
		return claudeNativeHeader{}, err
	}
	header := claudeNativeHeader{}
	for _, row := range rows {
		if sidechain, _ := row["isSidechain"].(bool); sidechain {
			continue
		}
		kind := nativeString(row, "type")
		if kind != "user" && kind != "assistant" {
			continue
		}
		if header.id == "" {
			header.id = nativeString(row, "sessionId", "session_id")
		}
		if header.cwd == "" {
			header.cwd = nativeString(row, "cwd")
		}
		message, _ := row["message"].(map[string]any)
		if header.preview == "" && kind == "user" && message != nil {
			preview, _, _ := nativeMessageContent(message["content"])
			header.preview = nativeSessionPreview(preview)
		}
		if header.model == "" && kind == "assistant" && message != nil {
			header.model = nativeString(message, "model")
		}
	}
	if header.id != "" && header.cwd != "" {
		return header, nil
	}
	return claudeNativeHeader{}, nativeSessionError(NativeSessionInvalidHistory, "Claude session metadata lacks session id or cwd")
}

func isClaudeTopLevelSessionPath(roots []string, path string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(rel) {
			continue
		}
		// ~/.claude/projects/<project>/<session>.jsonl only. Deeper files are
		// subagent or auxiliary transcripts and are never browsed recursively.
		return strings.Count(rel, string(filepath.Separator)) == 1
	}
	return false
}

func (b *claudeBackend) ListNativeSessions(_ context.Context, opts NativeSessionListOptions) (NativeSessionPage, error) {
	roots, err := b.claudeNativeRoots()
	if err != nil {
		return NativeSessionPage{}, err
	}
	if len(roots) == 0 {
		if _, err := parseNativeSessionCursor(opts.Cursor); err != nil {
			return NativeSessionPage{}, err
		}
		return NativeSessionPage{}, nil
	}
	return listNativeFileSessionsWhere(roots, opts, func(path string) bool {
		return isClaudeTopLevelSessionPath(roots, path)
	}, func(file nativeFile) (NativeSessionSummary, error) {
		header, err := parseClaudeNativeHeader(file.data)
		if err != nil {
			return NativeSessionSummary{}, err
		}
		title := nativeSessionTitle("Claude", header.cwd, header.id)
		if header.preview != "" {
			title = header.preview
		}
		return NativeSessionSummary{NativeID: header.id, Handle: file.path, Revision: file.revision, Title: title, Cwd: header.cwd, Preview: header.preview, UpdatedAt: file.updated, Model: header.model}, nil
	})
}

func claudeNativeNodes(rows []map[string]any) ([]nativeHistoryNode, error) {
	nodes := make([]nativeHistoryNode, 0, len(rows))
	for index, row := range rows {
		if sidechain, _ := row["isSidechain"].(bool); sidechain {
			continue
		}
		kind := nativeString(row, "type")
		if kind != "user" && kind != "assistant" {
			continue
		}
		id := nativeString(row, "uuid")
		if id == "" {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "Claude history entry %d lacks uuid", index)
		}
		parentID := ""
		if parent, exists := row["parentUuid"]; exists && parent != nil {
			var ok bool
			parentID, ok = parent.(string)
			if !ok {
				return nil, nativeSessionError(NativeSessionInvalidHistory, "Claude history entry %s has invalid parent", id)
			}
		}
		timestamp, err := nativeTimestamp(row, "timestamp")
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, nativeHistoryNode{id: id, parentID: parentID, timestamp: timestamp, raw: row})
	}
	return nodes, nil
}

func claudeNativeSnapshot(file nativeFile) (NativeSessionSnapshot, error) {
	rows, err := parseNativeJSONL(file.data)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	header, err := parseClaudeNativeHeader(file.data)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	cwd, err := nativeWorkdir(header.cwd)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	nodes, err := claudeNativeNodes(rows)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	branch, err := nativeActiveBranch(nodes)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	var messages []NativeHistoryMessage
	var warnings []string
	preview := ""
	model := ""
	for _, node := range branch {
		role := nativeString(node.raw, "type")
		message, ok := node.raw["message"].(map[string]any)
		if !ok {
			return NativeSessionSnapshot{}, nativeSessionError(NativeSessionInvalidHistory, "Claude history entry %s lacks message", node.id)
		}
		messages = appendNativeMessage(messages, role, node.id, node.timestamp, message, &warnings)
		if preview == "" && role == "user" {
			preview, _, _ = nativeMessageContent(message["content"])
		}
		if role == "assistant" {
			model = nativeString(message, "model")
		}
		if len(messages) > 10000 {
			return NativeSessionSnapshot{}, nativeSessionError(NativeSessionHistoryTooLarge, "Claude session exceeds 10000 messages")
		}
	}
	events := 0
	for _, message := range messages {
		events += len(message.Events)
	}
	if events > 50000 {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionHistoryTooLarge, "Claude session exceeds 50000 events")
	}
	return NativeSessionSnapshot{Summary: NativeSessionSummary{NativeID: header.id, Handle: file.path, Revision: file.revision, Title: nativeSessionTitle("Claude", cwd, header.id), Cwd: cwd, Preview: preview, UpdatedAt: file.updated, Model: model}, Messages: messages, Warnings: sortedUniqueWarnings(warnings)}, nil
}

func (b *claudeBackend) ReadNativeSession(_ context.Context, opts NativeSessionReadOptions) (NativeSessionSnapshot, error) {
	roots, err := b.claudeNativeRoots()
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	if len(roots) == 0 {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionNotFound, "Claude session root does not exist")
	}
	if !isClaudeTopLevelSessionPath(roots, opts.Handle) {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionNotFound, "Claude session is not a top-level project transcript")
	}
	file, err := readNativeFile(opts.Handle, roots, opts.Revision)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	return claudeNativeSnapshot(file)
}
