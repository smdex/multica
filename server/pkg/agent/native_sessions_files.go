package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const nativeSessionHeaderBytes = 64 << 10

type nativeFile struct {
	path     string
	root     string
	data     []byte
	revision string
	updated  time.Time
}

// openNativeFile opens a selected history through a descriptor rooted at a
// configured canonical root. nativeOpenRootFile rejects symlink components;
// it never trusts a canonicalized handle after opening it.
func openNativeFile(path string, roots []string) (*os.File, string, string, string, error) {
	root, relative, canonical, err := nativeRootRelativePath(path, roots)
	if err != nil {
		return nil, "", "", "", err
	}
	f, err := nativeOpenRootFile(root, relative)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", "", "", nativeSessionError(NativeSessionNotFound, "native session does not exist")
		}
		return nil, "", "", "", nativeSessionError(NativeSessionNotFound, "open native session safely: %v", err)
	}
	return f, root, relative, canonical, nil
}

func verifyNativeFile(root, relative string, f *os.File, before os.FileInfo) (os.FileInfo, error) {
	after, err := f.Stat()
	if err != nil {
		return nil, nativeSessionError(NativeSessionSourceChanged, "stat opened native session: %v", err)
	}
	if !before.Mode().IsRegular() || !after.Mode().IsRegular() || nativeSessionRevision(before) != nativeSessionRevision(after) {
		return nil, nativeSessionError(NativeSessionSourceChanged, "native session changed while reading")
	}
	// Re-open the final path through the same no-follow primitive. Comparing
	// identities catches a replacement after the original descriptor was
	// opened; comparing the revision catches a concurrent in-place write.
	current, err := nativeOpenRootFile(root, relative)
	if err != nil {
		return nil, nativeSessionError(NativeSessionSourceChanged, "native session changed while reading")
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil || !currentInfo.Mode().IsRegular() || !os.SameFile(before, currentInfo) || nativeSessionRevision(before) != nativeSessionRevision(currentInfo) {
		return nil, nativeSessionError(NativeSessionSourceChanged, "native session identity changed while reading")
	}
	return after, nil
}

func readNativeFile(path string, roots []string, expectedRevision string) (nativeFile, error) {
	f, root, relative, canonical, err := openNativeFile(path, roots)
	if err != nil {
		return nativeFile{}, err
	}
	defer f.Close()

	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nativeFile{}, nativeSessionError(NativeSessionNotFound, "native session is not a regular file")
	}
	if before.Size() > nativeSessionMaxBytes {
		return nativeFile{}, nativeSessionError(NativeSessionHistoryTooLarge, "native session exceeds %d bytes", nativeSessionMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, nativeSessionMaxBytes+1))
	if err != nil {
		return nativeFile{}, nativeSessionError(NativeSessionInvalidHistory, "read native session: %v", err)
	}
	if len(data) > nativeSessionMaxBytes {
		return nativeFile{}, nativeSessionError(NativeSessionHistoryTooLarge, "native session exceeds %d bytes", nativeSessionMaxBytes)
	}
	after, err := verifyNativeFile(root, relative, f, before)
	if err != nil {
		return nativeFile{}, err
	}
	revision := nativeSessionRevision(after)
	if expectedRevision != "" && expectedRevision != revision {
		return nativeFile{}, nativeSessionError(NativeSessionSourceChanged, "native session revision changed")
	}
	return nativeFile{path: canonical, root: root, data: data, revision: revision, updated: after.ModTime().UTC()}, nil
}

// readNativeFileHeader returns only the first 64 KiB and is the only helper
// list implementations may use for file metadata. It shares the no-follow,
// identity and revision checks with hydration without reading whole files.
func readNativeFileHeader(path string, roots []string) (nativeFile, error) {
	f, root, relative, canonical, err := openNativeFile(path, roots)
	if err != nil {
		return nativeFile{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nativeFile{}, nativeSessionError(NativeSessionNotFound, "native session is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, nativeSessionHeaderBytes+1))
	if err != nil {
		return nativeFile{}, nativeSessionError(NativeSessionInvalidHistory, "read native session header: %v", err)
	}
	if len(data) > nativeSessionHeaderBytes {
		data = data[:nativeSessionHeaderBytes]
	}
	after, err := verifyNativeFile(root, relative, f, before)
	if err != nil {
		return nativeFile{}, err
	}
	return nativeFile{path: canonical, root: root, data: data, revision: nativeSessionRevision(after), updated: after.ModTime().UTC()}, nil
}

func parseNativeJSONL(data []byte) ([]map[string]any, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), nativeSessionMaxLineBytes)
	var rows []map[string]any
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "invalid JSONL record: %v", err)
		}
		if row == nil {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "JSONL record is not an object")
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return nil, nativeSessionError(NativeSessionHistoryTooLarge, "native JSONL record exceeds %d bytes", nativeSessionMaxLineBytes)
		}
		return nil, nativeSessionError(NativeSessionInvalidHistory, "read native JSONL: %v", err)
	}
	if len(rows) == 0 {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "empty native session")
	}
	return rows, nil
}

func parseNativeFirstJSONLRecord(data []byte) (map[string]any, error) {
	line := data
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		line = data[:newline]
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "native session lacks a header")
	}
	var row map[string]any
	if err := json.Unmarshal(line, &row); err != nil || row == nil {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "invalid native session header")
	}
	return row, nil
}

// parseNativeHeaderJSONLRecords accepts a bounded prefix. A final partial
// record is intentionally ignored: listing must never turn a 64 KiB header
// budget into a full transcript read merely to complete that JSON object.
func parseNativeHeaderJSONLRecords(data []byte) ([]map[string]any, error) {
	var rows []map[string]any
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			break
		}
		line := bytes.TrimSpace(data[:newline])
		data = data[newline+1:]
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil || row == nil {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "invalid native session header record")
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "native session lacks complete header records")
	}
	return rows, nil
}

type nativeHistoryNode struct {
	id        string
	parentID  string
	timestamp time.Time
	raw       map[string]any
}

func nativeActiveBranch(nodes []nativeHistoryNode) ([]nativeHistoryNode, error) {
	if len(nodes) == 0 {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "native session has no history nodes")
	}
	byID := make(map[string]nativeHistoryNode, len(nodes))
	for _, node := range nodes {
		if node.id == "" {
			continue
		}
		if _, exists := byID[node.id]; exists {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "duplicate native history id")
		}
		byID[node.id] = node
	}
	leaf := nodes[len(nodes)-1]
	if leaf.id == "" {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "native history has no active node")
	}
	seen := make(map[string]bool, len(nodes))
	branch := make([]nativeHistoryNode, 0, len(nodes))
	for {
		if seen[leaf.id] {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "native history parent cycle")
		}
		seen[leaf.id] = true
		branch = append(branch, leaf)
		if leaf.parentID == "" {
			break
		}
		parent, ok := byID[leaf.parentID]
		if !ok {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "native history parent is missing")
		}
		leaf = parent
	}
	for left, right := 0, len(branch)-1; left < right; left, right = left+1, right-1 {
		branch[left], branch[right] = branch[right], branch[left]
	}
	return branch, nil
}

func nativeTimestamp(row map[string]any, field string) (time.Time, error) {
	value, _ := row[field].(string)
	if value == "" {
		return time.Time{}, nativeSessionError(NativeSessionInvalidHistory, "native record lacks %s", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, nativeSessionError(NativeSessionInvalidHistory, "invalid native timestamp: %v", err)
	}
	return parsed.UTC(), nil
}

func nativeString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := row[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nativeSessionPreview(value string) string {
	value = strings.TrimSpace(value)
	const limit = 240
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func nativeMessageContent(value any) (string, []Message, []string) {
	var text []string
	var events []Message
	var warnings []string
	appendText := func(value string) {
		if value != "" {
			text = append(text, value)
			events = append(events, Message{Type: MessageText, Content: value})
		}
	}
	var walk func(any)
	walk = func(item any) {
		switch item := item.(type) {
		case string:
			appendText(item)
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			kind := strings.ToLower(nativeString(item, "type", "kind"))
			switch kind {
			case "", "text":
				appendText(nativeString(item, "text", "content"))
			case "thinking", "reasoning":
				content := nativeString(item, "thinking", "text", "content")
				if content != "" {
					events = append(events, Message{Type: MessageThinking, Content: content})
				}
			case "toolcall", "tool_call", "tool_use":
				events = append(events, Message{Type: MessageToolUse, Tool: nativeString(item, "name", "toolName"), CallID: nativeString(item, "id", "toolCallId"), Input: nativeMap(item["arguments"], item["input"])})
			case "toolresult", "tool_result":
				events = append(events, Message{Type: MessageToolResult, Tool: nativeString(item, "name", "toolName"), CallID: nativeString(item, "toolCallId", "id"), Output: nativeString(item, "content", "text", "output")})
			default:
				warnings = append(warnings, "omitted unsupported historical block: "+kind)
			}
		default:
			warnings = append(warnings, "omitted non-text historical content")
		}
	}
	walk(value)
	return strings.Join(text, "\n"), events, warnings
}

func nativeMap(values ...any) map[string]any {
	for _, value := range values {
		if object, ok := value.(map[string]any); ok {
			return object
		}
	}
	return nil
}

func appendNativeMessage(messages []NativeHistoryMessage, role, nativeID string, createdAt time.Time, payload any, warnings *[]string) []NativeHistoryMessage {
	envelope, _ := payload.(map[string]any)
	if envelope != nil {
		payload = envelope["content"]
	}
	content, events, foundWarnings := nativeMessageContent(payload)
	*warnings = append(*warnings, foundWarnings...)
	switch role {
	case "user":
		return append(messages, NativeHistoryMessage{NativeID: nativeID, Role: role, Content: content, CreatedAt: createdAt})
	case "assistant":
		return append(messages, NativeHistoryMessage{NativeID: nativeID, Role: role, Content: content, CreatedAt: createdAt, Events: events})
	case "tool", "toolresult":
		if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
			*warnings = append(*warnings, "omitted historical tool result without an assistant message")
			return messages
		}
		toolResult := Message{Type: MessageToolResult, Content: content}
		if envelope != nil {
			toolResult.Tool = nativeString(envelope, "toolName", "name")
			toolResult.CallID = nativeString(envelope, "toolCallId", "id")
		}
		toolResult.Output = content
		messages[len(messages)-1].Events = append(messages[len(messages)-1].Events, toolResult)
		for _, event := range events {
			if event.Type != MessageText {
				messages[len(messages)-1].Events = append(messages[len(messages)-1].Events, event)
			}
		}
		return messages
	default:
		*warnings = append(*warnings, "omitted unsupported historical role: "+role)
		return messages
	}
}

func sortedUniqueWarnings(warnings []string) []string {
	if len(warnings) == 0 {
		return nil
	}
	sort.Strings(warnings)
	out := warnings[:0]
	for _, warning := range warnings {
		if len(out) == 0 || out[len(out)-1] != warning {
			out = append(out, warning)
		}
	}
	return out
}

func nativeWorkdir(cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", nativeSessionError(NativeSessionWorkdirUnavailable, "native working directory is not absolute")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", nativeSessionError(NativeSessionWorkdirUnavailable, "native working directory is unavailable")
	}
	return cwd, nil
}

func equivalentNativeHistory(source, owned NativeSessionSnapshot) bool {
	if len(source.Messages) != len(owned.Messages) {
		return false
	}
	for index := range source.Messages {
		left, right := source.Messages[index], owned.Messages[index]
		if left.Role != right.Role || left.Content != right.Content || !left.CreatedAt.Equal(right.CreatedAt) || !reflect.DeepEqual(left.Events, right.Events) {
			return false
		}
	}
	return true
}
