package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

const (
	workflowResultMaxBytes         = 24 << 20
	workflowCommandTimeout         = 15 * time.Second
	workflowListTimeout            = 30 * time.Second
	workflowImportTimeout          = 120 * time.Second
	workflowReportRetryDelay       = time.Second
	workflowClosedReportMaxRetries = 3
	workflowInteractionDeadline    = 15 * time.Minute
)

type workflowContextKey struct{}

func withWorkflowRun(ctx context.Context, run *workflowRun) context.Context {
	return context.WithValue(ctx, workflowContextKey{}, run)
}

func workflowRunFromContext(ctx context.Context) *workflowRun {
	run, _ := ctx.Value(workflowContextKey{}).(*workflowRun)
	return run
}

func newWorkflowRunID() string { return uuid.NewString() }

// workflowBackendForRuntime is deliberately a non-launching constructor. It
// receives only runtime configuration; default tests can replace it with a
// fake and must never discover an installed CLI or account.
func (d *Daemon) workflowBackendForRuntime(runtimeID string) (agent.Backend, Runtime, error) {
	rt := d.findRuntime(runtimeID)
	if rt == nil {
		return nil, Runtime{}, fmt.Errorf("runtime not found")
	}
	entry, ok := d.agents()[rt.Provider]
	usesCustomProfile := false
	var launchPrefix []string
	if profile, custom := d.customProfileLaunchForRuntime(rt.ID); custom {
		entry.Path = profile.path
		launchPrefix = profile.fixedArgs
		usesCustomProfile = true
	} else if !ok {
		return nil, Runtime{}, fmt.Errorf("provider %q is not available", rt.Provider)
	}
	backend, err := agent.ResolveBackend(rt.Provider, agent.Config{
		ExecutablePath: entry.Path,
		LaunchPrefix:   launchPrefix,
		Logger:         d.workflowLogger(),
		RuntimeID:      rt.ID,
		DaemonVersion:  d.cfg.CLIVersion,
		BuiltinRuntime: !usesCustomProfile,
	})
	if err != nil {
		return nil, Runtime{}, err
	}
	return backend, *rt, nil
}

func (d *Daemon) workflowLogger() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// agentWorkflowCapabilities never infers support from provider names. Native
// capabilities and static control support come from optional provider
// interfaces; a live Session separately supplies current run/turn admission.
func (d *Daemon) agentWorkflowCapabilities(runtimeID string) *protocol.AgentWorkflowCapabilities {
	if d.findRuntime(runtimeID) == nil {
		return nil
	}
	caps := &protocol.AgentWorkflowCapabilities{}
	if backend, _, err := d.workflowBackendForRuntime(runtimeID); err == nil {
		if _, ok := backend.(agent.NativeSessionProvider); ok {
			caps.NativeSessions.List = true
		}
		if _, ok := backend.(agent.NativeSessionImporter); ok {
			caps.NativeSessions.Import = true
		}
		if controls, ok := backend.(agent.InteractionCapabilityProvider); ok {
			declared := controls.InteractionCapabilities()
			caps.Controls.Steer = declared.Steer
			caps.Controls.Approvals = declared.Approvals
			caps.Controls.Questions = declared.Questions
		}
	}

	d.workflowMu.RLock()
	for _, run := range d.workflowRuns {
		if run.runtimeID != runtimeID {
			continue
		}
		steer, approvals, questions := run.controlCallbacks()
		caps.Controls.Steer = caps.Controls.Steer || steer
		caps.Controls.Approvals = caps.Controls.Approvals || approvals
		caps.Controls.Questions = caps.Controls.Questions || questions
	}
	d.workflowMu.RUnlock()
	return caps
}

func (d *Daemon) supportsWorkflowChat(runtimeID string) bool {
	backend, _, err := d.workflowBackendForRuntime(runtimeID)
	if err != nil {
		return false
	}
	controls, ok := backend.(agent.InteractionCapabilityProvider)
	if !ok {
		return false
	}
	declared := controls.InteractionCapabilities()
	return declared.Steer || declared.Approvals || declared.Questions
}

func validateWorkflowTaskPolicy(task *Task) error {
	if task.InteractionMode == "" {
		task.InteractionMode = "autonomous"
	}
	switch task.InteractionMode {
	case "autonomous", "chat":
	default:
		return fmt.Errorf("invalid interaction mode %q", task.InteractionMode)
	}
	if task.ResumePolicy == "" {
		task.ResumePolicy = "allow_fresh"
	}
	switch task.ResumePolicy {
	case "allow_fresh", "require_native":
	default:
		return fmt.Errorf("invalid resume policy %q", task.ResumePolicy)
	}
	if task.InteractionMode == "chat" && strings.TrimSpace(task.ChatSessionID) == "" {
		return errors.New("chat interaction mode requires a chat session")
	}
	if task.ResumePolicy == "require_native" && strings.TrimSpace(task.PriorSessionID) == "" {
		return errors.New("native resume policy requires a resume session")
	}
	return nil
}

func workflowServerSupports(resp *HeartbeatResponse, capability string) bool {
	if resp == nil {
		return false
	}
	for _, candidate := range resp.ServerCapabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (d *Daemon) handleAgentWorkflowCommands(ctx context.Context, runtimeID string, resp *HeartbeatResponse) {
	if resp == nil || len(resp.PendingAgentWorkflow) == 0 {
		return
	}
	for _, command := range resp.PendingAgentWorkflow {
		command := command
		if command.RuntimeID != runtimeID || strings.TrimSpace(command.ID) == "" {
			d.workflowLogger().Warn("ignoring malformed workflow command", "runtime_id", runtimeID, "request_id", command.ID)
			continue
		}
		capability := protocol.DaemonCapabilityNativeSessionImportV1
		if command.Kind == "steer" || command.Kind == "interaction_response" {
			capability = protocol.DaemonCapabilityChatControlsV1
		}
		if !workflowServerSupports(resp, capability) {
			d.workflowLogger().Debug("workflow command ignored: server capability was not negotiated", "runtime_id", runtimeID, "request_id", command.ID, "kind", command.Kind)
			continue
		}
		if !d.beginWorkflowDispatch(command.ID) {
			continue
		}
		go d.executeAgentWorkflowCommand(ctx, command)
	}
}

func (d *Daemon) beginWorkflowDispatch(commandID string) bool {
	d.workflowMu.Lock()
	defer d.workflowMu.Unlock()
	if d.workflowDispatches == nil {
		d.workflowDispatches = make(map[string]struct{})
	}
	if _, exists := d.workflowDispatches[commandID]; exists {
		return false
	}
	d.workflowDispatches[commandID] = struct{}{}
	return true
}

func (d *Daemon) executeAgentWorkflowCommand(parent context.Context, command protocol.AgentWorkflowCommand) {
	result := protocol.AgentWorkflowCommandResult{}
	if !command.ExpiresAt.IsZero() && !time.Now().Before(command.ExpiresAt) {
		result = workflowFailure("expired", "workflow command expired before local execution")
	} else {
		switch command.Kind {
		case "native_session_list":
			result = d.executeNativeSessionList(parent, command)
		case "native_session_import":
			result = d.executeNativeSessionImport(parent, command)
		case "steer":
			result = d.executeWorkflowSteer(parent, command)
		case "interaction_response":
			result = d.executeWorkflowInteractionResponse(parent, command)
		default:
			result = workflowFailure("invalid_request", "unsupported workflow command")
		}
	}
	if err := d.reportWorkflowCommandResult(parent, command.RuntimeID, command.ID, result); err != nil {
		d.workflowLogger().Error("workflow result was not acknowledged; durable retry queued", "runtime_id", command.RuntimeID, "request_id", command.ID, "error", err)
	}
}

func workflowFailure(code, message string) protocol.AgentWorkflowCommandResult {
	return protocol.AgentWorkflowCommandResult{Status: "failed", Error: &protocol.AgentWorkflowError{Code: code, Message: message}}
}

func workflowUnknown(code, message string) protocol.AgentWorkflowCommandResult {
	return protocol.AgentWorkflowCommandResult{Status: "unknown", Error: &protocol.AgentWorkflowError{Code: code, Message: message}}
}

func workflowCompleted(value any) protocol.AgentWorkflowCommandResult {
	body, err := json.Marshal(value)
	if err != nil {
		return workflowFailure("invalid_history", "could not encode workflow result")
	}
	if len(body) > workflowResultMaxBytes {
		return workflowFailure("history_too_large", "workflow result exceeds the report limit")
	}
	return protocol.AgentWorkflowCommandResult{Status: "completed", Result: body}
}

func workflowAgentError(err error) protocol.AgentWorkflowCommandResult {
	var nativeErr *agent.NativeSessionError
	if errors.As(err, &nativeErr) && nativeErr.Code != "" {
		return workflowFailure(nativeErr.Code, "native session operation failed")
	}
	return workflowFailure("invalid_history", "native session operation failed")
}

type workflowListBody struct {
	Cursor *string `json:"cursor"`
	Limit  int     `json:"limit"`
}

func (d *Daemon) executeNativeSessionList(parent context.Context, command protocol.AgentWorkflowCommand) protocol.AgentWorkflowCommandResult {
	var body workflowListBody
	if err := json.Unmarshal(command.Body, &body); err != nil {
		return workflowFailure("invalid_request", "invalid native session list command")
	}
	backend, _, err := d.workflowBackendForRuntime(command.RuntimeID)
	if err != nil {
		return workflowFailure("unsupported", "native session browsing is unavailable")
	}
	provider, ok := backend.(agent.NativeSessionProvider)
	if !ok {
		return workflowFailure("unsupported", "native session browsing is unavailable")
	}
	cursor := ""
	if body.Cursor != nil {
		cursor = *body.Cursor
	}
	ctx, cancel := context.WithTimeout(parent, workflowListTimeout)
	defer cancel()
	page, err := provider.ListNativeSessions(ctx, agent.NativeSessionListOptions{Cursor: cursor, Limit: body.Limit})
	if err != nil {
		return workflowAgentError(err)
	}
	type summary struct {
		NativeID  string    `json:"native_id"`
		Handle    string    `json:"handle"`
		Revision  string    `json:"revision"`
		Title     string    `json:"title"`
		Cwd       string    `json:"cwd"`
		Preview   string    `json:"preview"`
		UpdatedAt time.Time `json:"updated_at"`
		Model     string    `json:"model"`
	}
	items := make([]summary, 0, len(page.Sessions))
	for _, item := range page.Sessions {
		items = append(items, summary{item.NativeID, item.Handle, item.Revision, item.Title, item.Cwd, item.Preview, item.UpdatedAt, item.Model})
	}
	return workflowCompleted(struct {
		Sessions   []summary `json:"sessions"`
		NextCursor string    `json:"next_cursor"`
		Truncated  bool      `json:"truncated"`
	}{items, page.NextCursor, page.Truncated})
}

type workflowImportBody struct {
	ImportID      string `json:"import_id"`
	ChatSessionID string `json:"chat_session_id"`
	AgentID       string `json:"agent_id"`
	Handle        string `json:"handle"`
	Revision      string `json:"revision"`
	NativeID      string `json:"native_id"`
}

func (d *Daemon) executeNativeSessionImport(parent context.Context, command protocol.AgentWorkflowCommand) protocol.AgentWorkflowCommandResult {
	var body workflowImportBody
	if err := json.Unmarshal(command.Body, &body); err != nil || strings.TrimSpace(body.ImportID) == "" || strings.TrimSpace(body.ChatSessionID) == "" || strings.TrimSpace(body.AgentID) == "" || strings.TrimSpace(body.Handle) == "" || strings.TrimSpace(body.Revision) == "" {
		return workflowFailure("invalid_request", "invalid native session import command")
	}
	if _, err := uuid.Parse(body.ImportID); err != nil {
		return workflowFailure("invalid_request", "invalid native session import id")
	}
	if _, err := uuid.Parse(body.ChatSessionID); err != nil {
		return workflowFailure("invalid_request", "invalid native session import chat session")
	}
	if _, err := uuid.Parse(body.AgentID); err != nil {
		return workflowFailure("invalid_request", "invalid native session import agent")
	}
	backend, rt, err := d.workflowBackendForRuntime(command.RuntimeID)
	if err != nil {
		return workflowFailure("unsupported", "native session import is unavailable")
	}
	importer, ok := backend.(agent.NativeSessionImporter)
	if !ok {
		return workflowFailure("unsupported", "native session import is unavailable")
	}
	destination, err := d.nativeImportDestination(command.RuntimeID, body.ImportID)
	if err != nil {
		return workflowFailure("workdir_unavailable", "could not prepare owned native-session storage")
	}
	ctx, cancel := context.WithTimeout(parent, workflowImportTimeout)
	defer cancel()
	prepared, err := importer.PrepareNativeSession(ctx, agent.NativeSessionPrepareOptions{
		ImportID: body.ImportID, Handle: body.Handle, Revision: body.Revision, DestinationDir: destination,
	})
	if err != nil {
		return workflowAgentError(err)
	}
	if strings.TrimSpace(body.NativeID) == "" || strings.TrimSpace(prepared.ResumeSessionID) == "" || strings.TrimSpace(prepared.ResumeCwd) == "" || prepared.Snapshot.Summary.NativeID == body.NativeID {
		return workflowFailure("resume_unavailable", "provider did not prepare a distinct resumable native session")
	}
	if strings.TrimSpace(prepared.Snapshot.Summary.Handle) == "" || strings.TrimSpace(prepared.Snapshot.Summary.Revision) == "" {
		return workflowFailure("resume_unavailable", "provider did not identify the owned native session revision")
	}
	providerHome, err := nativeImportProviderHome(rt.Provider)
	if err != nil {
		return workflowFailure("resume_unavailable", "could not resolve provider home for imported native session")
	}
	if err := d.persistNativeImportContext(nativeImportContext{
		Version:         1,
		RuntimeID:       command.RuntimeID,
		ImportID:        body.ImportID,
		ChatSessionID:   body.ChatSessionID,
		AgentID:         body.AgentID,
		SourceNativeID:  body.NativeID,
		OwnedNativeID:   prepared.Snapshot.Summary.NativeID,
		OwnedHandle:     prepared.Snapshot.Summary.Handle,
		OwnedRevision:   prepared.Snapshot.Summary.Revision,
		DestinationDir:  destination,
		ProviderHome:    providerHome,
		ResumeSessionID: prepared.ResumeSessionID,
		ResumeCwd:       prepared.ResumeCwd,
	}); err != nil {
		return workflowFailure("resume_unavailable", "could not persist imported native session context")
	}
	return workflowCompleted(struct {
		NativeID        string                   `json:"native_id"`
		OwnedNativeID   string                   `json:"owned_native_id"`
		Provider        string                   `json:"provider"`
		ResumeSessionID string                   `json:"resume_session_id"`
		WorkDir         string                   `json:"work_dir"`
		Messages        []workflowHistoryMessage `json:"messages"`
		Warnings        []string                 `json:"warnings"`
	}{
		// native_id stays the immutable source identity used by the server's
		// idempotency/deduplication record. The fork identity is separate and
		// is never mistaken for the selected source on a replay.
		NativeID: body.NativeID, OwnedNativeID: prepared.Snapshot.Summary.NativeID, Provider: rt.Provider,
		ResumeSessionID: prepared.ResumeSessionID, WorkDir: prepared.ResumeCwd,
		Messages: workflowHistoryMessages(prepared.Snapshot.Messages), Warnings: prepared.Snapshot.Warnings,
	})
}

// nativeImportContext is the daemon-owned proof that a require_native task
// refers to a fork created on this machine, not a source identity supplied
// through a later task claim. It also carries the validated original cwd, so
// resumed execution never points at the private preparation directory.
type nativeImportContext struct {
	Version         int    `json:"version"`
	RuntimeID       string `json:"runtime_id"`
	ImportID        string `json:"import_id"`
	ChatSessionID   string `json:"chat_session_id"`
	AgentID         string `json:"agent_id"`
	SourceNativeID  string `json:"source_native_id"`
	OwnedNativeID   string `json:"owned_native_id"`
	OwnedHandle     string `json:"owned_handle"`
	OwnedRevision   string `json:"owned_revision"`
	DestinationDir  string `json:"destination_dir"`
	ProviderHome    string `json:"provider_home,omitempty"`
	ResumeSessionID string `json:"resume_session_id"`
	ResumeCwd       string `json:"resume_cwd"`
}

func nativeImportProviderHome(provider string) (string, error) {
	if provider != "codex" {
		return "", nil
	}
	return execenv.CanonicalSharedCodexHome()
}

func (d *Daemon) nativeImportContextPath(runtimeID, resumeSessionID string) (string, error) {
	if strings.TrimSpace(d.cfg.WorkspacesRoot) == "" || strings.TrimSpace(runtimeID) == "" || strings.TrimSpace(resumeSessionID) == "" {
		return "", errors.New("native import context is incomplete")
	}
	sum := sha256.Sum256([]byte(runtimeID + "\x00" + resumeSessionID))
	return filepath.Join(d.cfg.WorkspacesRoot, ".native-session-import-index", "v1", hex.EncodeToString(sum[:])+".json"), nil
}

func (d *Daemon) persistNativeImportContext(record nativeImportContext) error {
	return d.storeNativeImportContext(record, nil)
}

// advanceNativeImportContextRevision records a revision observed immediately
// after this daemon's own completed provider execution. The expected record
// check prevents a stale execution from accepting an intervening external
// mutation as its own write.
func (d *Daemon) advanceNativeImportContextRevision(record nativeImportContext, revision string) error {
	if strings.TrimSpace(revision) == "" {
		return errors.New("owned native session has no revision")
	}
	if revision == record.OwnedRevision {
		return nil
	}
	next := record
	next.OwnedRevision = revision
	return d.storeNativeImportContext(next, &record)
}

func validNativeImportContext(record nativeImportContext) bool {
	return record.Version == 1 &&
		strings.TrimSpace(record.RuntimeID) != "" &&
		strings.TrimSpace(record.ImportID) != "" &&
		strings.TrimSpace(record.ChatSessionID) != "" &&
		strings.TrimSpace(record.AgentID) != "" &&
		strings.TrimSpace(record.SourceNativeID) != "" &&
		strings.TrimSpace(record.OwnedNativeID) != "" &&
		strings.TrimSpace(record.OwnedHandle) != "" &&
		strings.TrimSpace(record.OwnedRevision) != "" &&
		(record.ProviderHome == "" || filepath.IsAbs(record.ProviderHome)) &&
		filepath.IsAbs(record.DestinationDir) &&
		strings.TrimSpace(record.ResumeSessionID) != "" &&
		strings.TrimSpace(record.ResumeCwd) != "" &&
		record.SourceNativeID != record.OwnedNativeID
}

func validateNativeImportTaskBinding(task Task, record nativeImportContext) error {
	if task.AgentID != record.AgentID || task.ChatSessionID != record.ChatSessionID {
		return errors.New("required native import is not bound to this agent chat session")
	}
	return nil
}

// storeNativeImportContext either creates an idempotent import record or,
// with expected set, atomically advances its observed owned revision.
func (d *Daemon) storeNativeImportContext(record nativeImportContext, expected *nativeImportContext) error {
	if !validNativeImportContext(record) {
		return errors.New("native import context is incomplete")
	}
	if err := requireCanonicalNativeImportDestination(record.DestinationDir); err != nil {
		return err
	}
	path, err := d.nativeImportContextPath(record.RuntimeID, record.ResumeSessionID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if body, err := os.ReadFile(path); err == nil {
		var existing nativeImportContext
		if json.Unmarshal(body, &existing) != nil {
			return errors.New("native import context is invalid")
		}
		if expected == nil {
			if !reflect.DeepEqual(existing, record) {
				return errors.New("native import context conflicts with existing fork")
			}
			return nil
		}
		if !reflect.DeepEqual(existing, *expected) {
			return errors.New("native import context changed during execution")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if expected != nil {
		return errors.New("native import context disappeared during execution")
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".native-import-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncTerminalReportDir(dir)
}

func (d *Daemon) loadNativeImportContext(runtimeID, resumeSessionID string) (nativeImportContext, error) {
	path, err := d.nativeImportContextPath(runtimeID, resumeSessionID)
	if err != nil {
		return nativeImportContext{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nativeImportContext{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nativeImportContext{}, errors.New("native import context is not a regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nativeImportContext{}, err
	}
	var record nativeImportContext
	if err := json.Unmarshal(body, &record); err != nil || record.RuntimeID != runtimeID || record.ResumeSessionID != resumeSessionID || !validNativeImportContext(record) {
		return nativeImportContext{}, errors.New("native import context is invalid")
	}
	if err := requireCanonicalNativeImportDestination(record.DestinationDir); err != nil {
		return nativeImportContext{}, err
	}
	return record, nil
}

func requireCanonicalNativeImportDestination(destination string) error {
	clean := filepath.Clean(destination)
	if !filepath.IsAbs(clean) {
		return errors.New("owned import directory is not absolute")
	}
	canonical, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("canonicalize owned import directory: %w", err)
	}
	info, err := os.Lstat(canonical)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("owned import directory is not canonical")
	}
	if canonical != clean {
		return errors.New("owned import directory changed from its canonical path")
	}
	return nil
}

func verifyNativeImportRevision(ctx context.Context, backend agent.Backend, record nativeImportContext) error {
	snapshot, err := readOwnedNativeSession(ctx, backend, record, record.OwnedRevision)
	if err != nil {
		return fmt.Errorf("read owned native session: %w", err)
	}
	if snapshot.Summary.NativeID != record.OwnedNativeID || snapshot.Summary.Handle != record.OwnedHandle || snapshot.Summary.Revision != record.OwnedRevision || !sameExistingDir(snapshot.Summary.Cwd, record.ResumeCwd) {
		return errors.New("owned native session no longer matches the imported identity")
	}
	return nil
}

func (d *Daemon) refreshNativeImportRevision(ctx context.Context, backend agent.Backend, record nativeImportContext) error {
	snapshot, err := readOwnedNativeSession(ctx, backend, record, "")
	if err != nil {
		return fmt.Errorf("read completed owned native session: %w", err)
	}
	if snapshot.Summary.NativeID != record.OwnedNativeID || snapshot.Summary.Handle != record.OwnedHandle || strings.TrimSpace(snapshot.Summary.Revision) == "" || !sameExistingDir(snapshot.Summary.Cwd, record.ResumeCwd) {
		return errors.New("completed owned native session no longer matches the imported identity")
	}
	return d.advanceNativeImportContextRevision(record, snapshot.Summary.Revision)
}

// readOwnedNativeSession accepts an owned-session reader when a provider keeps
// its fork outside the browsable user history roots. The destination comes only
// from the daemon's durable preparation record; it is never reconstructed from
// a browser handle or task input. Providers without that optional boundary
// retain their existing native-session reader.
func readOwnedNativeSession(ctx context.Context, backend agent.Backend, record nativeImportContext, revision string) (agent.NativeSessionSnapshot, error) {
	read := agent.NativeSessionReadOptions{Handle: record.OwnedHandle, Revision: revision}
	if owned, ok := backend.(agent.NativeOwnedSessionProvider); ok {
		return owned.ReadOwnedNativeSession(ctx, agent.NativeOwnedSessionReadOptions{
			NativeSessionReadOptions: read,
			DestinationDir:           record.DestinationDir,
			NativeID:                 record.OwnedNativeID,
		})
	}
	provider, ok := backend.(agent.NativeSessionProvider)
	if !ok {
		return agent.NativeSessionSnapshot{}, errors.New("provider cannot read the owned native session")
	}
	return provider.ReadNativeSession(ctx, read)
}

// acquireNativeResumeCwdLock gives an imported chat the same canonical-path
// guard used by local_directory tasks. Ordinary chat tasks may bypass that
// guard for responsive short turns; imported sessions cannot, because their
// provider resume contract pins the original native cwd.
func (d *Daemon) acquireNativeResumeCwdLock(ctx context.Context, taskID, cwd string) (func(), error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve native import cwd: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat native import cwd: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("native import cwd is not a directory")
	}
	if d.localPathLocks == nil {
		return nil, errors.New("native import cwd lock is not configured")
	}
	release, err := d.localPathLocks.Acquire(ctx, filepath.Clean(resolved), taskID, nil)
	if err != nil {
		return nil, err
	}
	return release, nil
}

func (d *Daemon) nativeImportDestination(runtimeID, importID string) (string, error) {
	if strings.TrimSpace(d.cfg.WorkspacesRoot) == "" {
		return "", errors.New("workspaces root is not configured")
	}
	sum := sha256.Sum256([]byte(runtimeID + "\x00" + importID))
	dir := filepath.Join(d.cfg.WorkspacesRoot, ".native-session-imports", "v1", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("owned import directory is invalid")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("canonicalize owned import directory: %w", err)
	}
	if err := requireCanonicalNativeImportDestination(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

type workflowHistoryMessage struct {
	NativeID  string                    `json:"native_id"`
	Role      string                    `json:"role"`
	Content   string                    `json:"content"`
	CreatedAt time.Time                 `json:"created_at"`
	Events    []workflowHistoricalEvent `json:"events"`
}

type workflowHistoricalEvent struct {
	Seq             int            `json:"seq"`
	Type            string         `json:"type"`
	Tool            string         `json:"tool,omitempty"`
	Content         string         `json:"content,omitempty"`
	Input           map[string]any `json:"input,omitempty"`
	Output          string         `json:"output,omitempty"`
	OutputTruncated *bool          `json:"output_truncated,omitempty"`
	CreatedAt       *time.Time     `json:"created_at,omitempty"`
}

func workflowHistoryMessages(messages []agent.NativeHistoryMessage) []workflowHistoryMessage {
	out := make([]workflowHistoryMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		out = append(out, workflowHistoryMessage{
			NativeID: message.NativeID, Role: message.Role, Content: message.Content, CreatedAt: message.CreatedAt,
			Events: workflowHistoricalEvents(message.Events),
		})
	}
	return out
}

func workflowHistoricalEvents(messages []agent.Message) []workflowHistoricalEvent {
	out := make([]workflowHistoricalEvent, 0, len(messages))
	for _, message := range messages {
		event := workflowHistoricalEvent{Seq: len(out) + 1}
		switch message.Type {
		case agent.MessageText:
			event.Type, event.Content = "text", message.Content
		case agent.MessageThinking:
			event.Type, event.Content = "thinking", message.Content
		case agent.MessageToolUse:
			event.Type, event.Tool, event.Input = "tool_use", message.Tool, redact.InputMap(message.Input)
		case agent.MessageToolResult:
			event.Type, event.Tool, event.Output = "tool_result", message.Tool, message.Output
		case agent.MessageError:
			event.Type, event.Content = "error", message.Content
		default:
			continue
		}
		out = append(out, event)
	}
	return out
}

type workflowSteerBody struct {
	ChatSessionID string `json:"chat_session_id"`
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	TurnID        string `json:"turn_id"`
	Content       string `json:"content"`
}

func (d *Daemon) executeWorkflowSteer(parent context.Context, command protocol.AgentWorkflowCommand) protocol.AgentWorkflowCommandResult {
	var body workflowSteerBody
	if err := json.Unmarshal(command.Body, &body); err != nil || body.TaskID == "" || body.RunID == "" || body.TurnID == "" || strings.TrimSpace(body.Content) == "" {
		return workflowFailure("invalid_request", "invalid steer command")
	}
	run := d.workflowRun(body.TaskID, body.RunID, command.RuntimeID)
	if run == nil {
		return workflowFailure("stale_turn", "the requested run is no longer active")
	}
	ctx, cancel := context.WithTimeout(parent, workflowCommandTimeout)
	defer cancel()
	delivery, err := run.steer(ctx, command.ID, body.TurnID, body.Content)
	return workflowDeliveryResult(delivery, err)
}

type workflowInteractionResponseBody struct {
	ChatSessionID string                                    `json:"chat_session_id"`
	TaskID        string                                    `json:"task_id"`
	RunID         string                                    `json:"run_id"`
	TurnID        string                                    `json:"turn_id"`
	InteractionID string                                    `json:"interaction_id"`
	Response      protocol.AgentWorkflowInteractionResponse `json:"response"`
}

func (d *Daemon) executeWorkflowInteractionResponse(parent context.Context, command protocol.AgentWorkflowCommand) protocol.AgentWorkflowCommandResult {
	var body workflowInteractionResponseBody
	if err := json.Unmarshal(command.Body, &body); err != nil || body.TaskID == "" || body.RunID == "" || body.TurnID == "" || body.InteractionID == "" {
		return workflowFailure("invalid_request", "invalid interaction response command")
	}
	run := d.workflowRun(body.TaskID, body.RunID, command.RuntimeID)
	if run == nil {
		return workflowFailure("stale_turn", "the requested run is no longer active")
	}
	answers := make([]agent.InteractionAnswer, 0, len(body.Response.Answers))
	for _, answer := range body.Response.Answers {
		answers = append(answers, agent.InteractionAnswer{QuestionID: answer.QuestionID, OptionIDs: answer.OptionIDs, Text: answer.Text})
	}
	ctx, cancel := context.WithTimeout(parent, workflowCommandTimeout)
	defer cancel()
	delivery, err := run.respond(ctx, agent.InteractionResponse{
		ID: command.ID, InteractionID: body.InteractionID, ExpectedTurnID: body.TurnID,
		ChoiceID: body.Response.ChoiceID, Answers: answers, Cancelled: body.Response.Cancelled,
	})
	return workflowDeliveryResult(delivery, err)
}

func workflowDeliveryResult(delivery agent.InputDelivery, err error) protocol.AgentWorkflowCommandResult {
	result := workflowCompleted(struct {
		Delivery string `json:"delivery"`
		Code     string `json:"code,omitempty"`
	}{Delivery: delivery.State, Code: delivery.Code})
	if err != nil || delivery.State == "unknown" || delivery.State == "" {
		code := delivery.Code
		if code == "" {
			code = "delivery_unknown"
		}
		return protocol.AgentWorkflowCommandResult{
			Status: "unknown", Result: result.Result,
			Error: &protocol.AgentWorkflowError{Code: code, Message: "provider delivery could not be confirmed"},
		}
	}
	if delivery.State == "rejected" {
		result.Status = "failed"
		result.Error = &protocol.AgentWorkflowError{Code: delivery.Code, Message: "provider rejected the command"}
		if result.Error.Code == "" {
			result.Error.Code = "rejected"
		}
	}
	return result
}

type workflowRun struct {
	daemon    *Daemon
	taskID    string
	runtimeID string
	runID     string

	mu           sync.Mutex
	session      *agent.Session
	closed       bool
	state        protocol.TaskControlState
	stateDirty   bool
	interactions []protocol.AgentWorkflowInteractionRequest
	pending      map[string]agent.InteractionRequest
	resolving    map[string]string
	expiring     map[string]bool
	wake         chan struct{}
}

func (d *Daemon) registerWorkflowRun(task Task, runID string) *workflowRun {
	if task.InteractionMode != "chat" {
		return nil
	}
	run := &workflowRun{
		daemon: d, taskID: task.ID, runtimeID: task.RuntimeID, runID: runID,
		state: protocol.TaskControlState{RunID: runID}, pending: make(map[string]agent.InteractionRequest), resolving: make(map[string]string), expiring: make(map[string]bool), wake: make(chan struct{}, 1),
	}
	d.workflowMu.Lock()
	if d.workflowRuns == nil {
		d.workflowRuns = make(map[string]*workflowRun)
	}
	d.workflowRuns[task.ID] = run
	d.workflowMu.Unlock()
	go run.reportLoop()
	return run
}

func (d *Daemon) workflowRun(taskID, runID, runtimeID string) *workflowRun {
	d.workflowMu.RLock()
	run := d.workflowRuns[taskID]
	d.workflowMu.RUnlock()
	if run == nil || run.runID != runID || run.runtimeID != runtimeID {
		return nil
	}
	return run
}

func (d *Daemon) unregisterWorkflowRun(run *workflowRun) {
	if run == nil {
		return
	}
	run.close()
	d.workflowMu.Lock()
	if d.workflowRuns[run.taskID] == run {
		delete(d.workflowRuns, run.taskID)
	}
	d.workflowMu.Unlock()
}

func (d *Daemon) cancelWorkflowRun(taskID string) {
	d.workflowMu.RLock()
	run := d.workflowRuns[taskID]
	d.workflowMu.RUnlock()
	if run != nil {
		run.close()
	}
}

func (r *workflowRun) controlCallbacks() (steer, approvals, questions bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.session == nil {
		return false, false, false
	}
	return r.session.Steer != nil, r.session.RespondToInteraction != nil, r.session.RespondToInteraction != nil
}

func (r *workflowRun) bind(session *agent.Session) {
	if session == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.session = session
	r.setStateLocked(sessionControlState(session))
	r.mu.Unlock()
	r.signal()
}

func sessionControlState(session *agent.Session) agent.ControlState {
	if session != nil && session.ControlState != nil {
		return session.ControlState()
	}
	return agent.ControlState{}
}

func (r *workflowRun) updateControl(state agent.ControlState) {
	r.mu.Lock()
	if !r.closed {
		r.setStateLocked(state)
	}
	r.mu.Unlock()
	r.signal()
}

func (r *workflowRun) setStateLocked(state agent.ControlState) {
	var turnID *string
	if state.TurnID != "" {
		turn := state.TurnID
		turnID = &turn
	}
	r.state = protocol.TaskControlState{
		RunID: r.runID, TurnID: turnID, Active: state.Active,
		CanSteer:   state.Active && state.CanSteer && r.session != nil && r.session.Steer != nil,
		CanApprove: state.Active && state.CanApprove && r.session != nil && r.session.RespondToInteraction != nil,
		CanAnswer:  state.Active && state.CanAnswer && r.session != nil && r.session.RespondToInteraction != nil,
	}
	r.stateDirty = true
}

func (r *workflowRun) presentInteraction(request agent.InteractionRequest) {
	if request.ID == "" || request.TurnID == "" || (request.Kind != "approval" && request.Kind != "question") {
		return
	}
	r.mu.Lock()
	if r.closed || r.session == nil {
		r.mu.Unlock()
		return
	}
	if _, exists := r.pending[request.ID]; exists {
		r.mu.Unlock()
		return
	}
	expiresAt, err := normalizeWorkflowInteractionDeadline(request.ExpiresAt, time.Now())
	if err != nil {
		session := r.session
		r.mu.Unlock()
		r.rejectInvalidInteractionDeadline(session, request, err)
		return
	}
	request.ExpiresAt = expiresAt
	r.pending[request.ID] = request
	r.interactions = append(r.interactions, workflowInteractionRequest(request))
	r.mu.Unlock()
	r.signal()
	go r.expireInteraction(request.ID, request.ExpiresAt)
}

// normalizeWorkflowInteractionDeadline assigns one bounded deadline at the
// daemon admission boundary. Provider adapters deliberately leave deadlines
// unset because they do not share the server's durable interaction lifecycle;
// retries must reuse this exact value rather than extending human admission.
func normalizeWorkflowInteractionDeadline(expiresAt, now time.Time) (time.Time, error) {
	if expiresAt.IsZero() {
		return now.Add(workflowInteractionDeadline).UTC(), nil
	}
	if !expiresAt.After(now) {
		return time.Time{}, errors.New("deadline is already expired")
	}
	if expiresAt.After(now.Add(workflowInteractionDeadline)) {
		return time.Time{}, fmt.Errorf("deadline exceeds the maximum interaction admission of %s", workflowInteractionDeadline)
	}
	if _, err := expiresAt.MarshalJSON(); err != nil {
		return time.Time{}, fmt.Errorf("deadline cannot be serialized: %w", err)
	}
	return expiresAt, nil
}

// rejectInvalidInteractionDeadline fails closed before an invalid report can
// enter the retry loop. The provider gets a single cancellation, while the
// warning makes the rejected admission diagnosable without exposing its input.
func (r *workflowRun) rejectInvalidInteractionDeadline(session *agent.Session, request agent.InteractionRequest, err error) {
	if r.daemon != nil {
		r.daemon.workflowLogger().Warn("rejecting provider interaction with invalid interaction deadline", "task_id", r.taskID, "run_id", r.runID, "interaction_id", request.ID, "error", err)
	}
	if session == nil || session.RespondToInteraction == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.liveContext(), workflowCommandTimeout)
	defer cancel()
	if _, responseErr := session.RespondToInteraction(ctx, agent.InteractionResponse{
		ID: "invalid-deadline:" + request.ID, InteractionID: request.ID, ExpectedTurnID: request.TurnID, Cancelled: true,
	}); responseErr != nil && r.daemon != nil {
		r.daemon.workflowLogger().Debug("reject invalid provider interaction deadline", "task_id", r.taskID, "run_id", r.runID, "interaction_id", request.ID, "error", responseErr)
	}
}

func workflowInteractionRequest(request agent.InteractionRequest) protocol.AgentWorkflowInteractionRequest {
	choices := make([]protocol.AgentWorkflowInteractionChoice, 0, len(request.Choices))
	for _, choice := range request.Choices {
		choices = append(choices, protocol.AgentWorkflowInteractionChoice{ID: choice.ID, Label: choice.Label})
	}
	questions := make([]protocol.AgentWorkflowInteractionQuestion, 0, len(request.Questions))
	for _, question := range request.Questions {
		options := make([]protocol.AgentWorkflowInteractionOption, 0, len(question.Options))
		for _, option := range question.Options {
			options = append(options, protocol.AgentWorkflowInteractionOption{ID: option.ID, Label: option.Label, Description: option.Description})
		}
		questions = append(questions, protocol.AgentWorkflowInteractionQuestion{ID: question.ID, Prompt: question.Prompt, Options: options, Multiple: question.Multiple, AllowText: question.AllowText, Secret: question.Secret})
	}
	return protocol.AgentWorkflowInteractionRequest{
		ID: request.ID, TurnID: request.TurnID, Kind: request.Kind, Title: request.Title, Description: request.Description,
		Tool: request.Tool, Input: redact.InputMap(request.Input), Choices: choices, Questions: questions, ExpiresAt: request.ExpiresAt,
	}
}

func (r *workflowRun) expireInteraction(interactionID string, expiresAt time.Time) {
	if wait := time.Until(expiresAt); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		<-timer.C
	}
	r.mu.Lock()
	request, exists := r.pending[interactionID]
	if !exists || r.closed {
		r.mu.Unlock()
		return
	}
	r.removeQueuedInteractionLocked(interactionID)
	r.stateDirty = true
	if _, resolving := r.resolving[interactionID]; resolving {
		// A human response has entered the provider gate. Let it settle before
		// deciding whether an expiry denial is still necessary; otherwise two
		// competing writes could reach the same provider prompt.
		r.expiring[interactionID] = true
		r.mu.Unlock()
		r.signal()
		return
	}
	delete(r.pending, interactionID)
	session := r.session
	r.mu.Unlock()
	r.signal()
	if session == nil || session.RespondToInteraction == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.liveContext(), workflowCommandTimeout)
	defer cancel()
	// This is a one-shot denial through the same provider gate as a human
	// response. An uncertain write is intentionally not retried.
	_, _ = session.RespondToInteraction(ctx, agent.InteractionResponse{
		ID: "expiry:" + interactionID, InteractionID: interactionID, ExpectedTurnID: request.TurnID, Cancelled: true,
	})
}

func (r *workflowRun) removeQueuedInteractionLocked(id string) {
	for index, interaction := range r.interactions {
		if interaction.ID == id {
			r.interactions = append(r.interactions[:index], r.interactions[index+1:]...)
			return
		}
	}
}

func (r *workflowRun) steer(ctx context.Context, commandID, turnID, content string) (agent.InputDelivery, error) {
	r.mu.Lock()
	if r.closed || r.session == nil || r.session.Steer == nil {
		r.mu.Unlock()
		return agent.InputDelivery{State: "rejected", Code: "stale_turn"}, nil
	}
	session := r.session
	r.mu.Unlock()
	return session.Steer(ctx, agent.SteerRequest{ID: commandID, ExpectedTurnID: turnID, Content: content})
}

func (r *workflowRun) respond(ctx context.Context, response agent.InteractionResponse) (agent.InputDelivery, error) {
	r.mu.Lock()
	request, exists := r.pending[response.InteractionID]
	if r.closed || !exists || request.TurnID != response.ExpectedTurnID || r.session == nil || r.session.RespondToInteraction == nil {
		r.mu.Unlock()
		return agent.InputDelivery{State: "rejected", Code: "stale_turn"}, nil
	}
	if _, resolving := r.resolving[response.InteractionID]; resolving {
		r.mu.Unlock()
		return agent.InputDelivery{State: "rejected", Code: "interaction_resolving"}, nil
	}
	r.resolving[response.InteractionID] = response.ID
	session := r.session
	r.mu.Unlock()

	delivery, err := session.RespondToInteraction(ctx, response)
	shouldExpire := false
	r.mu.Lock()
	delete(r.resolving, response.InteractionID)
	if !r.closed {
		switch {
		case err == nil && delivery.State == "rejected":
			// A definite rejection did not consume the provider prompt. Keep it
			// pending so the server can present it again; an uncertain delivery is
			// deliberately never retried or answered again.
			if r.expiring[response.InteractionID] {
				delete(r.pending, response.InteractionID)
				delete(r.expiring, response.InteractionID)
				shouldExpire = true
			}
		default:
			delete(r.pending, response.InteractionID)
			delete(r.expiring, response.InteractionID)
		}
	}
	r.mu.Unlock()
	if shouldExpire {
		go r.expireResolvedInteraction(request)
	}
	return delivery, err
}

func (r *workflowRun) expireResolvedInteraction(request agent.InteractionRequest) {
	r.mu.Lock()
	if r.closed || r.session == nil || r.session.RespondToInteraction == nil {
		r.mu.Unlock()
		return
	}
	session := r.session
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.liveContext(), workflowCommandTimeout)
	defer cancel()
	_, _ = session.RespondToInteraction(ctx, agent.InteractionResponse{
		ID: "expiry:" + request.ID, InteractionID: request.ID, ExpectedTurnID: request.TurnID, Cancelled: true,
	})
}

func (r *workflowRun) waitingForHuman() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && len(r.pending) > 0
}

func (r *workflowRun) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	session := r.session
	r.pending = make(map[string]agent.InteractionRequest)
	r.resolving = make(map[string]string)
	r.expiring = make(map[string]bool)
	// A run that has ended must not publish a pending human prompt after its
	// terminal state. The server treats those prompts as current admission.
	r.interactions = nil
	r.setStateLocked(agent.ControlState{})
	r.mu.Unlock()
	r.signal()
	if session != nil && session.CancelPendingInputs != nil {
		ctx, cancel := context.WithTimeout(r.liveContext(), workflowCommandTimeout)
		defer cancel()
		if err := session.CancelPendingInputs(ctx); err != nil {
			r.daemon.workflowLogger().Debug("cancel pending provider inputs failed", "task_id", r.taskID, "run_id", r.runID, "error", err)
		}
	}
}

func (r *workflowRun) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *workflowRun) liveContext() context.Context {
	if r.daemon != nil {
		return r.daemon.recoveryContext()
	}
	return context.Background()
}

func (r *workflowRun) nextReport() (protocol.TaskControlState, bool, protocol.AgentWorkflowInteractionRequest, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stateDirty {
		state := r.state
		r.stateDirty = false
		return state, true, protocol.AgentWorkflowInteractionRequest{}, false, false
	}
	if len(r.interactions) > 0 {
		return protocol.TaskControlState{}, false, r.interactions[0], true, false
	}
	return protocol.TaskControlState{}, false, protocol.AgentWorkflowInteractionRequest{}, false, r.closed
}

func (r *workflowRun) acknowledgeInteraction(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.interactions) > 0 && r.interactions[0].ID == id {
		r.interactions = r.interactions[1:]
	}
}

func (r *workflowRun) interactionQueued(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, interaction := range r.interactions {
		if interaction.ID == id {
			return true
		}
	}
	return false
}

func (r *workflowRun) reportLoop() {
	closedFailures := 0
	for {
		<-r.wake
		for {
			state, hasState, interaction, hasInteraction, done := r.nextReport()
			if done {
				return
			}
			if !hasState && !hasInteraction {
				break
			}
			ctx, cancel := context.WithTimeout(r.liveContext(), workflowCommandTimeout)
			var err error
			if hasState {
				err = r.daemon.client.ReportTaskControlState(ctx, r.taskID, state)
			} else {
				err = r.daemon.client.ReportTaskInteraction(ctx, r.taskID, r.runID, interaction)
			}
			cancel()
			if err != nil {
				r.daemon.workflowLogger().Debug("workflow live report failed; retrying", "task_id", r.taskID, "run_id", r.runID, "error", err)
				if hasState {
					r.mu.Lock()
					r.stateDirty = true
					r.mu.Unlock()
				}
				if hasInteraction && !r.interactionQueued(interaction.ID) {
					// The deadline passed while this report was in flight. Do not
					// retry an interaction the server will reject; the expiry path
					// has already queued a current control-state reconciliation.
					continue
				}
				if r.closedLiveReportFailure(err) {
					return
				}
				if r.isClosed() {
					closedFailures++
					if closedFailures >= workflowClosedReportMaxRetries {
						r.daemon.workflowLogger().Debug("dropping final live workflow report after bounded retries", "task_id", r.taskID, "run_id", r.runID)
						return
					}
				} else {
					closedFailures = 0
				}
				timer := time.NewTimer(workflowReportRetryDelay)
				<-timer.C
				continue
			}
			if hasInteraction {
				r.acknowledgeInteraction(interaction.ID)
			}
		}
	}
}

func (r *workflowRun) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *workflowRun) closedLiveReportFailure(err error) bool {
	if !r.isClosed() {
		return false
	}
	var requestErr *requestError
	return errors.As(err, &requestErr) && (requestErr.StatusCode == http.StatusNotFound || requestErr.StatusCode == http.StatusConflict)
}

// workflowReportStore is a compact durable outbox for final workflow command
// outcomes. It contains no bearer token or provider callback state, only the
// immutable request identity and the exact report safe to replay.
type workflowReportStore struct {
	dir string
	mu  sync.Mutex
}

type workflowReportRecord struct {
	Version   int                                 `json:"version"`
	RuntimeID string                              `json:"runtime_id"`
	RequestID string                              `json:"request_id"`
	Result    protocol.AgentWorkflowCommandResult `json:"result"`
}

func newWorkflowReportStore(cfg Config) *workflowReportStore {
	if strings.TrimSpace(cfg.WorkspacesRoot) == "" {
		return nil
	}
	identity := strings.TrimRight(cfg.ServerBaseURL, "/") + "\x00" + cfg.Profile + "\x00" + cfg.DaemonID
	sum := sha256.Sum256([]byte(identity))
	return &workflowReportStore{dir: filepath.Join(cfg.WorkspacesRoot, ".pending-agent-workflow-reports", "v1", hex.EncodeToString(sum[:16]))}
}

func workflowReportFileName(runtimeID, requestID string) string {
	sum := sha256.Sum256([]byte(runtimeID + "\x00" + requestID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func (s *workflowReportStore) ensure() error {
	if s == nil {
		return errors.New("workflow report store is not configured")
	}
	return ensureTerminalReportDir(s.dir)
}

func (s *workflowReportStore) enqueue(record workflowReportRecord) error {
	if strings.TrimSpace(record.RuntimeID) == "" || strings.TrimSpace(record.RequestID) == "" {
		return errors.New("workflow report has no runtime or request id")
	}
	if record.Version == 0 {
		record.Version = 1
	}
	if record.Version != 1 {
		return errors.New("unsupported workflow report version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return err
	}
	name := workflowReportFileName(record.RuntimeID, record.RequestID)
	path := filepath.Join(s.dir, name)
	if body, err := os.ReadFile(path); err == nil {
		var existing workflowReportRecord
		if err := json.Unmarshal(body, &existing); err != nil || !reflect.DeepEqual(existing, record) {
			return errors.New("workflow report conflicts with existing payload")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".workflow-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncTerminalReportDir(s.dir)
}

func (s *workflowReportStore) list() ([]workflowReportRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]workflowReportRecord, 0, len(entries))
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("read workflow report %s: %w", entry.Name(), err))
			continue
		}
		var record workflowReportRecord
		if err := json.Unmarshal(body, &record); err != nil {
			errs = append(errs, fmt.Errorf("decode workflow report %s: %w", entry.Name(), err))
			continue
		}
		if record.Version != 1 {
			errs = append(errs, fmt.Errorf("workflow report %s has unsupported version %d", entry.Name(), record.Version))
			continue
		}
		if entry.Name() != workflowReportFileName(record.RuntimeID, record.RequestID) {
			errs = append(errs, fmt.Errorf("workflow report %s does not match runtime/request identity", entry.Name()))
			continue
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out, errors.Join(errs...)
}

func (s *workflowReportStore) acknowledge(record workflowReportRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, workflowReportFileName(record.RuntimeID, record.RequestID))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncTerminalReportDir(s.dir)
}

func (d *Daemon) reportWorkflowCommandResult(parent context.Context, runtimeID, requestID string, result protocol.AgentWorkflowCommandResult) error {
	record := workflowReportRecord{Version: 1, RuntimeID: runtimeID, RequestID: requestID, Result: result}
	persisted := false
	if d.workflowReports != nil {
		if err := d.workflowReports.enqueue(record); err != nil {
			d.workflowLogger().Error("persist workflow result; continuing with direct delivery", "runtime_id", runtimeID, "request_id", requestID, "error", err)
		} else {
			persisted = true
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
	defer cancel()
	err := d.client.postJSONWithRetry(ctx, fmt.Sprintf("/api/daemon/runtimes/%s/agent-workflow-requests/%s/result", runtimeID, requestID), result, nil, defaultTerminalRetrySchedule)
	if err != nil {
		if persisted {
			d.signalWorkflowReportReplay()
		}
		return err
	}
	if persisted {
		if err := d.workflowReports.acknowledge(record); err != nil {
			d.signalWorkflowReportReplay()
			return err
		}
	}
	return nil
}

func (d *Daemon) signalWorkflowReportReplay() {
	if d.workflowReportWakeup == nil {
		return
	}
	select {
	case d.workflowReportWakeup <- struct{}{}:
	default:
	}
}

func (d *Daemon) workflowReportReplayLoop(ctx context.Context) {
	if d.workflowReports == nil {
		return
	}
	d.signalWorkflowReportReplay()
	ticker := time.NewTicker(workflowReportRetryDelay)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.workflowReportWakeup:
		}
		reports, err := d.workflowReports.list()
		if err != nil {
			d.workflowLogger().Error("load pending workflow reports; preserving invalid artifacts", "error", err)
		}
		if len(reports) == 0 {
			continue
		}
		for _, report := range reports {
			callCtx, cancel := context.WithTimeout(ctx, workflowCommandTimeout)
			err := d.client.ReportAgentWorkflowResult(callCtx, report.RuntimeID, report.RequestID, report.Result)
			cancel()
			if err != nil {
				d.workflowLogger().Warn("pending workflow report remains queued", "runtime_id", report.RuntimeID, "request_id", report.RequestID, "error", err)
				continue
			}
			if err := d.workflowReports.acknowledge(report); err != nil {
				d.workflowLogger().Error("acknowledge replayed workflow report; retaining for retry", "runtime_id", report.RuntimeID, "request_id", report.RequestID, "error", err)
			}
		}
	}
}
