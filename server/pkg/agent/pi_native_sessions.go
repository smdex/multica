//go:build linux || darwin

package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const piNativePreparationTimeout = 20 * time.Second

type piNativeHeader struct {
	id  string
	cwd string
}

func (b *piBackend) piNativeRoots() ([]string, error) {
	if b.providerLabel == "omp" {
		return nil, nativeSessionError(NativeSessionUnsupported, "omp has no verified native history contract")
	}
	home, err := effectiveNativeHome(b.cfg)
	if err != nil {
		return nil, err
	}
	expandHome := func(path string) string {
		if path == "~" {
			return home
		}
		if strings.HasPrefix(path, "~/") {
			return filepath.Join(home, path[2:])
		}
		return path
	}
	rootPath := strings.TrimSpace(effectiveNativeEnv(b.cfg, "PI_CODING_AGENT_SESSION_DIR"))
	if rootPath != "" {
		rootPath = expandHome(rootPath)
	} else {
		agentDir := strings.TrimSpace(effectiveNativeEnv(b.cfg, "PI_CODING_AGENT_DIR"))
		if agentDir == "" {
			agentDir = filepath.Join(home, ".pi", "agent")
		} else {
			agentDir = expandHome(agentDir)
		}
		rootPath = filepath.Join(agentDir, "sessions")
	}
	root, err := canonicalNativeRoot(rootPath)
	if err != nil {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "canonicalize Pi session root: %v", err)
	}
	if root == "" {
		return nil, nil
	}
	return []string{root}, nil
}

func parsePiNativeHeader(data []byte) (piNativeHeader, error) {
	row, err := parseNativeFirstJSONLRecord(data)
	if err != nil {
		return piNativeHeader{}, err
	}
	if row["type"] != "session" {
		return piNativeHeader{}, nativeSessionError(NativeSessionInvalidHistory, "Pi session lacks a session header")
	}
	header := piNativeHeader{id: nativeString(row, "id"), cwd: nativeString(row, "cwd")}
	if header.id == "" || header.cwd == "" {
		return piNativeHeader{}, nativeSessionError(NativeSessionInvalidHistory, "Pi session header lacks id or cwd")
	}
	return header, nil
}

func (b *piBackend) ListNativeSessions(_ context.Context, opts NativeSessionListOptions) (NativeSessionPage, error) {
	roots, err := b.piNativeRoots()
	if err != nil {
		return NativeSessionPage{}, err
	}
	if len(roots) == 0 {
		if _, err := parseNativeSessionCursor(opts.Cursor); err != nil {
			return NativeSessionPage{}, err
		}
		return NativeSessionPage{}, nil
	}
	return listNativeFileSessions(roots, opts, func(file nativeFile) (NativeSessionSummary, error) {
		header, err := parsePiNativeHeader(file.data)
		if err != nil {
			return NativeSessionSummary{}, err
		}
		return NativeSessionSummary{
			NativeID:  nativeSessionIdentity(file.root, header.id),
			Handle:    file.path,
			Revision:  file.revision,
			Title:     nativeSessionTitle("Pi", header.cwd, header.id),
			Cwd:       header.cwd,
			UpdatedAt: file.updated,
		}, nil
	})
}

func piNativeNodes(rows []map[string]any) ([]nativeHistoryNode, error) {
	if len(rows) == 0 {
		return nil, nativeSessionError(NativeSessionInvalidHistory, "empty Pi session")
	}
	nodes := make([]nativeHistoryNode, 0, len(rows)-1)
	for index, row := range rows[1:] {
		id := nativeString(row, "id")
		if id == "" {
			return nil, nativeSessionError(NativeSessionInvalidHistory, "Pi session entry %d lacks id", index+1)
		}
		parentID := ""
		if rawParent, exists := row["parentId"]; exists && rawParent != nil {
			var ok bool
			parentID, ok = rawParent.(string)
			if !ok {
				return nil, nativeSessionError(NativeSessionInvalidHistory, "Pi session entry %s has invalid parent", id)
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

func piNativeSnapshot(file nativeFile) (NativeSessionSnapshot, piNativeHeader, error) {
	rows, err := parseNativeJSONL(file.data)
	if err != nil {
		return NativeSessionSnapshot{}, piNativeHeader{}, err
	}
	header, err := parsePiNativeHeader(file.data)
	if err != nil {
		return NativeSessionSnapshot{}, piNativeHeader{}, err
	}
	cwd, err := nativeWorkdir(header.cwd)
	if err != nil {
		return NativeSessionSnapshot{}, piNativeHeader{}, err
	}
	nodes, err := piNativeNodes(rows)
	if err != nil {
		return NativeSessionSnapshot{}, piNativeHeader{}, err
	}
	branch, err := nativeActiveBranch(nodes)
	if err != nil {
		return NativeSessionSnapshot{}, piNativeHeader{}, err
	}
	var messages []NativeHistoryMessage
	var warnings []string
	model := ""
	for _, node := range branch {
		kind := nativeString(node.raw, "type")
		switch kind {
		case "message":
			message, ok := node.raw["message"].(map[string]any)
			if !ok {
				return NativeSessionSnapshot{}, piNativeHeader{}, nativeSessionError(NativeSessionInvalidHistory, "Pi message %s is invalid", node.id)
			}
			role := strings.ToLower(nativeString(message, "role"))
			if role == "" {
				return NativeSessionSnapshot{}, piNativeHeader{}, nativeSessionError(NativeSessionInvalidHistory, "Pi message %s lacks role", node.id)
			}
			messages = appendNativeMessage(messages, role, node.id, node.timestamp, message, &warnings)
			if model == "" && role == "assistant" {
				model = nativeString(message, "model")
			}
		case "model_change":
			model = nativeString(node.raw, "modelId", "model")
		}
		if len(messages) > 10000 {
			return NativeSessionSnapshot{}, piNativeHeader{}, nativeSessionError(NativeSessionHistoryTooLarge, "Pi session exceeds 10000 messages")
		}
	}
	events := 0
	preview := ""
	for _, message := range messages {
		events += len(message.Events)
		if preview == "" && message.Role == "user" {
			preview = message.Content
		}
	}
	if events > 50000 {
		return NativeSessionSnapshot{}, piNativeHeader{}, nativeSessionError(NativeSessionHistoryTooLarge, "Pi session exceeds 50000 events")
	}
	return NativeSessionSnapshot{
		Summary: NativeSessionSummary{
			NativeID:  nativeSessionIdentity(file.root, header.id),
			Handle:    file.path,
			Revision:  file.revision,
			Title:     nativeSessionTitle("Pi", cwd, header.id),
			Cwd:       cwd,
			Preview:   preview,
			UpdatedAt: file.updated,
			Model:     model,
		},
		// A browsed source is never an execution target. Preparation overrides
		// this only after a distinct native clone has been proven and hydrated.
		ResumeSessionID: "",
		Messages:        messages,
		Warnings:        sortedUniqueWarnings(warnings),
	}, header, nil
}

func (b *piBackend) ReadNativeSession(_ context.Context, opts NativeSessionReadOptions) (NativeSessionSnapshot, error) {
	roots, err := b.piNativeRoots()
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	if len(roots) == 0 {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionNotFound, "Pi session root does not exist")
	}
	file, err := readNativeFile(opts.Handle, roots, opts.Revision)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	snapshot, _, err := piNativeSnapshot(file)
	return snapshot, err
}

func (b *piBackend) ReadOwnedNativeSession(_ context.Context, opts NativeOwnedSessionReadOptions) (NativeSessionSnapshot, error) {
	if b.providerLabel == "omp" || !filepath.IsAbs(opts.DestinationDir) || opts.NativeID == "" {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionResumeUnavailable, "owned Pi read requires bound directory and identity")
	}
	// Do not resolve a symlink here: the daemon persists the canonical root.
	file, err := readNativeFile(opts.Handle, []string{filepath.Clean(opts.DestinationDir)}, opts.Revision)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	snapshot, _, err := piNativeSnapshot(file)
	if err != nil {
		return NativeSessionSnapshot{}, err
	}
	if snapshot.Summary.NativeID != opts.NativeID {
		return NativeSessionSnapshot{}, nativeSessionError(NativeSessionSourceChanged, "owned Pi identity changed")
	}
	return snapshot, nil
}

func piActiveBranchFingerprint(data []byte) (string, error) {
	rows, err := parseNativeJSONL(data)
	if err != nil {
		return "", err
	}
	if _, err := parsePiNativeHeader(data); err != nil {
		return "", err
	}
	nodes, err := piNativeNodes(rows)
	if err != nil {
		return "", err
	}
	branch, err := nativeActiveBranch(nodes)
	if err != nil {
		return "", err
	}
	normalized := make([]map[string]any, 0, len(branch))
	replacements := map[string]string{}
	retained := map[string]bool{}
	var pending []string
	for _, node := range branch {
		if nativeString(node.raw, "type") == "label" {
			pending = append(pending, node.id)
			continue
		}
		retained[node.id] = true
		for _, id := range pending {
			replacements[id] = node.id
		}
		pending = nil
	}
	labels := map[string]string{}
	for _, row := range rows[1:] {
		if nativeString(row, "type") != "label" {
			continue
		}
		target := nativeString(row, "targetId")
		if !retained[target] {
			continue
		}
		if label := nativeString(row, "label"); label != "" {
			labels[target] = label
		} else {
			delete(labels, target)
		}
	}
	for _, node := range branch {
		if nativeString(node.raw, "type") == "label" {
			continue
		}
		copyBytes, err := json.Marshal(node.raw)
		if err != nil {
			return "", nativeSessionError(NativeSessionInvalidHistory, "copy Pi active branch: %v", err)
		}
		var copyRow map[string]any
		if err := json.Unmarshal(copyBytes, &copyRow); err != nil {
			return "", nativeSessionError(NativeSessionInvalidHistory, "decode Pi active branch: %v", err)
		}
		if nativeString(copyRow, "type") == "compaction" {
			if replacement, ok := replacements[nativeString(copyRow, "firstKeptEntryId")]; ok {
				copyRow["firstKeptEntryId"] = replacement
			}
		}
		delete(copyRow, "id")
		delete(copyRow, "parentId")
		delete(copyRow, "timestamp")
		normalized = append(normalized, copyRow)
	}
	encoded, err := json.Marshal([]any{normalized, labels})
	if err != nil {
		return "", nativeSessionError(NativeSessionInvalidHistory, "encode Pi active branch: %v", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

type piNativeRPCResponse struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   json.RawMessage `json:"error"`
}

type piNativeRPC struct {
	stdin      io.WriteCloser
	responses  chan piNativeRPCResponse
	done       <-chan struct{}
	nextID     int
	mu         sync.Mutex
	expectedMu sync.RWMutex
	expected   string
}

func (c *piNativeRPC) request(ctx context.Context, command string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := fmt.Sprintf("multica-native-%d", c.nextID)
	c.expectedMu.Lock()
	c.expected = id
	c.expectedMu.Unlock()
	defer func() {
		c.expectedMu.Lock()
		c.expected = ""
		c.expectedMu.Unlock()
	}()
	frame, err := json.Marshal(map[string]any{"id": id, "type": command})
	if err != nil {
		return nil, err
	}
	frame = append(frame, '\n')
	if _, err := c.stdin.Write(frame); err != nil {
		return nil, nativeSessionError(NativeSessionResumeUnavailable, "write Pi %s request: %v", command, err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, nativeSessionError(NativeSessionResumeUnavailable, "Pi %s request: %v", command, ctx.Err())
		case <-c.done:
			return nil, nativeSessionError(NativeSessionResumeUnavailable, "Pi exited during %s", command)
		case response, ok := <-c.responses:
			if !ok {
				return nil, nativeSessionError(NativeSessionResumeUnavailable, "Pi exited during %s", command)
			}
			if response.ID != id || response.Command != command || response.Type != "response" {
				continue
			}
			if !response.Success {
				return nil, nativeSessionError(NativeSessionResumeUnavailable, "Pi %s failed: %s", command, strings.TrimSpace(string(response.Error)))
			}
			return response.Data, nil
		}
	}
}

func (b *piBackend) startPiNativeRPC(ctx context.Context, stage, cwd string) (*piNativeRPC, func(), error) {
	execName := b.cfg.ExecutablePath
	if execName == "" {
		execName = b.defaultExecutable
	}
	if execName == "" {
		execName = "pi"
	}
	lookedUp, err := exec.LookPath(execName)
	if err != nil {
		return nil, nil, nativeSessionError(NativeSessionUnsupported, "Pi executable not found at %q: %v", execName, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, piNativePreparationTimeout)
	args := []string{"--mode", "rpc", "--session", stage, "--session-dir", filepath.Dir(stage), "--no-extensions"}
	cmd, _, _ := b.cfg.commandAt(execName).execVia(runCtx, choosePiInvocation, lookedUp, args, b.cfg.Logger)
	cmd.Dir = cwd
	cmd.Env = buildEnv(b.cfg.Env)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, nil, nativeSessionError(NativeSessionResumeUnavailable, "open Pi RPC stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cancel()
		return nil, nil, nativeSessionError(NativeSessionResumeUnavailable, "open Pi RPC stdout: %v", err)
	}
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cancel()
		return nil, nil, nativeSessionError(NativeSessionResumeUnavailable, "start Pi RPC: %v", err)
	}
	responses := make(chan piNativeRPCResponse, 1)
	readerDone := make(chan struct{})
	client := &piNativeRPC{stdin: stdin, responses: responses, done: readerDone}
	go func() {
		defer close(readerDone)
		defer close(responses)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), nativeSessionMaxLineBytes)
		for scanner.Scan() {
			var response piNativeRPCResponse
			if json.Unmarshal(scanner.Bytes(), &response) != nil || response.Type != "response" {
				continue
			}
			client.expectedMu.RLock()
			expected := client.expected
			client.expectedMu.RUnlock()
			if response.ID == "" || response.ID != expected {
				continue
			}
			select {
			case responses <- response:
			case <-runCtx.Done():
				return
			}
		}
	}()
	cleanup := func() {
		cancel()
		_ = stdin.Close()
		signalProcessGroup(cmd, syscall.SIGKILL)
		_ = stdout.Close()
		_ = cmd.Wait()
		<-readerDone
		releaseProcessGroup(cmd)
	}
	return client, cleanup, nil
}

func (b *piBackend) preparePiFromManifest(ctx context.Context, manifest nativePreparationManifest, opts NativeSessionPrepareOptions) (PreparedNativeSession, error) {
	if !nativeManifestMatches(manifest, opts, "pi") {
		return PreparedNativeSession{}, nativeSessionError(NativeSessionSourceChanged, "native preparation id belongs to a different source")
	}
	file, err := readNativeFile(manifest.OwnedHandle, []string{filepath.Clean(opts.DestinationDir)}, manifest.OwnedRevision)
	if err != nil {
		return PreparedNativeSession{}, err
	}
	snapshot, _, err := piNativeSnapshot(file)
	if err != nil {
		return PreparedNativeSession{}, err
	}
	if manifest.ResumeSession != manifest.OwnedHandle || snapshot.Summary.Cwd != manifest.ResumeCwd || snapshot.Summary.Handle != manifest.OwnedHandle || snapshot.Summary.Revision != manifest.OwnedRevision {
		return PreparedNativeSession{}, nativeSessionError(NativeSessionSourceChanged, "owned Pi session changed")
	}
	snapshot.ResumeSessionID = manifest.ResumeSession
	return PreparedNativeSession{Snapshot: snapshot, ResumeSessionID: manifest.ResumeSession, ResumeCwd: manifest.ResumeCwd}, nil
}

func (b *piBackend) PrepareNativeSession(ctx context.Context, opts NativeSessionPrepareOptions) (PreparedNativeSession, error) {
	roots, err := b.piNativeRoots()
	if err != nil {
		return PreparedNativeSession{}, err
	}
	if len(roots) == 0 {
		return PreparedNativeSession{}, nativeSessionError(NativeSessionNotFound, "Pi session root does not exist")
	}
	manifestPath, stagePath, err := nativePreparationPaths(opts, "pi")
	if err != nil {
		return PreparedNativeSession{}, err
	}
	ownedRoot, err := canonicalNativeRoot(opts.DestinationDir)
	if err != nil || ownedRoot == "" {
		return PreparedNativeSession{}, nativeSessionError(NativeSessionResumeUnavailable, "resolve owned Pi directory")
	}
	opts.DestinationDir = ownedRoot
	manifestPath = filepath.Join(ownedRoot, filepath.Base(manifestPath))
	stagePath = filepath.Join(ownedRoot, filepath.Base(stagePath))
	var prepared PreparedNativeSession
	err = withNativePreparationLock(manifestPath, func() error {
		manifest, exists, manifestErr := readNativePreparationManifest(manifestPath)
		if manifestErr != nil {
			return manifestErr
		}
		if exists {
			result, err := b.preparePiFromManifest(ctx, manifest, opts)
			prepared = result
			return err
		}

		sourceFile, err := readNativeFile(opts.Handle, roots, opts.Revision)
		if err != nil {
			return err
		}
		sourceSnapshot, sourceHeader, err := piNativeSnapshot(sourceFile)
		if err != nil {
			return err
		}
		sourceFingerprint, err := piActiveBranchFingerprint(sourceFile.data)
		if err != nil {
			return err
		}
		intentPath := nativePreparationIntentPath(stagePath)
		intent, hasIntent, err := readNativePreparationIntent(intentPath)
		if err != nil {
			return err
		}
		if hasIntent {
			if !nativeIntentMatches(intent, opts, "pi") {
				return nativeSessionError(NativeSessionSourceChanged, "native preparation intent belongs to a different source")
			}
			switch intent.Phase {
			case "owned":
				manifest = nativePreparationManifest{ImportID: opts.ImportID, Provider: "pi", SourceHandle: opts.Handle, SourceRevision: opts.Revision, OwnedHandle: intent.OwnedHandle, OwnedRevision: intent.OwnedRevision, ResumeSession: intent.ResumeSession, ResumeCwd: intent.ResumeCwd}
				result, err := b.preparePiFromManifest(ctx, manifest, opts)
				if err != nil {
					return err
				}
				if err := writeNativePreparationManifest(manifestPath, manifest); err != nil {
					return err
				}
				prepared = result
				return nil
			case "cloning":
				return nativeSessionError(NativeSessionResumeUnavailable, "Pi native preparation stopped during clone; refusing an ambiguous second fork")
			case "staged":
				staged, err := readNativeStage(stagePath)
				if err != nil || !bytes.Equal(staged, sourceFile.data) {
					return nativeSessionError(NativeSessionResumeUnavailable, "Pi native staging copy cannot be safely recovered")
				}
			default:
				return nativeSessionError(NativeSessionResumeUnavailable, "Pi native preparation has an unknown recovery phase")
			}
		} else {
			if _, err := os.Lstat(stagePath); err == nil {
				return nativeSessionError(NativeSessionResumeUnavailable, "Pi native staging copy has no durable intent; refusing an ambiguous fork")
			} else if !os.IsNotExist(err) {
				return nativeSessionError(NativeSessionInvalidHistory, "inspect Pi native staging copy: %v", err)
			}
			if err := writeNativeStage(stagePath, sourceFile.data); err != nil {
				return err
			}
			intent = nativePreparationIntent{ImportID: opts.ImportID, Provider: "pi", SourceHandle: opts.Handle, SourceRevision: opts.Revision, Phase: "staged"}
			if err := writeNativeAtomicJSON(intentPath, intent); err != nil {
				return err
			}
		}
		intent.Phase = "cloning"
		if err := writeNativeAtomicJSON(intentPath, intent); err != nil {
			return err
		}
		client, cleanup, err := b.startPiNativeRPC(ctx, stagePath, sourceSnapshot.Summary.Cwd)
		if err != nil {
			return err
		}
		defer cleanup()
		cloneData, err := client.request(ctx, "clone")
		if err != nil {
			return err
		}
		var cloneResult struct {
			Cancelled bool `json:"cancelled"`
		}
		if len(cloneData) != 0 && json.Unmarshal(cloneData, &cloneResult) != nil {
			return nativeSessionError(NativeSessionResumeUnavailable, "invalid Pi clone response")
		}
		if cloneResult.Cancelled {
			return nativeSessionError(NativeSessionResumeUnavailable, "Pi clone was cancelled")
		}
		stateData, err := client.request(ctx, "get_state")
		if err != nil {
			return err
		}
		var state struct {
			SessionFile string `json:"sessionFile"`
			SessionID   string `json:"sessionId"`
			IsStreaming bool   `json:"isStreaming"`
		}
		if err := json.Unmarshal(stateData, &state); err != nil || state.SessionFile == "" || state.SessionID == "" || state.IsStreaming {
			return nativeSessionError(NativeSessionResumeUnavailable, "Pi clone did not report a stable native session")
		}
		ownedFile, err := readNativeFile(state.SessionFile, []string{filepath.Dir(stagePath)}, "")
		if err != nil {
			return err
		}
		if ownedFile.path == sourceFile.path || ownedFile.path == stagePath {
			return nativeSessionError(NativeSessionResumeUnavailable, "Pi clone retained the source session path")
		}
		ownedSnapshot, ownedHeader, err := piNativeSnapshot(ownedFile)
		if err != nil {
			return err
		}
		if ownedHeader.id == sourceHeader.id || ownedHeader.id != state.SessionID {
			return nativeSessionError(NativeSessionResumeUnavailable, "Pi clone did not create a distinct native identity")
		}
		ownedFingerprint, err := piActiveBranchFingerprint(ownedFile.data)
		if err != nil {
			return err
		}
		if sourceFingerprint != ownedFingerprint || !equivalentNativeHistory(sourceSnapshot, ownedSnapshot) {
			return nativeSessionError(NativeSessionResumeUnavailable, "Pi clone does not preserve the selected native branch")
		}
		ownedSnapshot.ResumeSessionID = ownedFile.path
		intent.Phase = "owned"
		intent.OwnedHandle = ownedFile.path
		intent.OwnedRevision = ownedFile.revision
		intent.ResumeSession = ownedFile.path
		intent.ResumeCwd = ownedSnapshot.Summary.Cwd
		if err := writeNativeAtomicJSON(intentPath, intent); err != nil {
			return err
		}
		manifest = nativePreparationManifest{
			ImportID:       opts.ImportID,
			Provider:       "pi",
			SourceHandle:   opts.Handle,
			SourceRevision: opts.Revision,
			OwnedHandle:    ownedFile.path,
			OwnedRevision:  ownedFile.revision,
			ResumeSession:  ownedFile.path,
			ResumeCwd:      ownedSnapshot.Summary.Cwd,
		}
		if err := writeNativePreparationManifest(manifestPath, manifest); err != nil {
			return err
		}
		prepared = PreparedNativeSession{Snapshot: ownedSnapshot, ResumeSessionID: ownedFile.path, ResumeCwd: ownedSnapshot.Summary.Cwd}
		return nil
	})
	if err != nil {
		return PreparedNativeSession{}, err
	}
	return prepared, nil
}
