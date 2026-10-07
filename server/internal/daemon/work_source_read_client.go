package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// Source-read transport constants mirroring the server contract in
// server/internal/handler/source_read_token.go and work_source_command.go.
const (
	// msrTokenPrefix is the marker for minted source-read capabilities.
	msrTokenPrefix = "msr_"
	// sourceReadScope is the only scope the mint route accepts.
	sourceReadScope = "source:read"
	// sourceReadMaxTTL bounds expires_in / expires_at distance at the client
	// boundary; the server mints 120-second capabilities.
	sourceReadMaxTTL = 120 * time.Second
	// sourceReadResultMaxBytes bounds the result payload (2 MiB).
	sourceReadResultMaxBytes = 2 << 20
	// sourceReadErrorMaxBytes bounds the failure diagnostic (8 KiB).
	sourceReadErrorMaxBytes = 8 << 10
)

// SourceReadCredential is a short-lived, runtime-scoped read capability. It
// never replaces or mutates the client's human token; callers pass it
// explicitly to ListSourceReadCommands/ClaimSourceReadCommand/
// ReportSourceReadCommand.
type SourceReadCredential struct {
	Token       string
	ExpiresAt   time.Time
	ExpiresIn   int64
	RuntimeID   string
	WorkspaceID string
}

// SourceReadCommand mirrors the pending/claim/result receipt JSON. Result and
// Error are server-marshaled JSON strings, not decoded objects.
type SourceReadCommand struct {
	ID               string
	RequestID        string
	WorkspaceID      string
	SourceID         string
	SourceHandle     string
	Command          string
	NativeID         string
	LimitCount       int32
	ConfigRevision   int32
	ExpiresAt        string
	Status           string
	ClaimedRuntimeID string
	Result           string
	Error            string
}

// sourceReadTokenResponse is the mint route's JSON body.
type sourceReadTokenResponse struct {
	Token       string `json:"token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	RuntimeID   string `json:"runtime_id"`
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	ExpiresAt   string `json:"expires_at"`
	ExpiresIn   int64  `json:"expires_in"`
}

// sourceReadCommandDTO matches the pending-list response (a superset of the
// claim/receipt response; those simply omit source_handle).
type sourceReadCommandDTO struct {
	ID           string `json:"id"`
	RequestID    string `json:"request_id"`
	WorkspaceID  string `json:"workspace_id"`
	SourceID     string `json:"source_id"`
	SourceHandle string `json:"source_handle"`
	Command      string `json:"command"`
	NativeID     string `json:"native_id"`
	LimitCount   int32  `json:"limit_count"`
	ConfigRev    int32  `json:"config_revision"`
	ExpiresAt    string `json:"expires_at"`
	Status       string `json:"status"`
	ClaimedRunID string `json:"claimed_runtime_id"`
	ClaimedAt    string `json:"claimed_at"`
	Result       string `json:"result"`
	Error        string `json:"error"`
	CreatedBy    string `json:"created_by"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// validateSourceReadIDs rejects blank, malformed, or non-canonical UUID
// selectors before any HTTP request is made.
func validateSourceReadIDs(fields map[string]string) error {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateSourceReadUUID(name, fields[name]); err != nil {
			return err
		}
	}
	return nil
}

// validateSourceReadUUID rejects blank, malformed, or non-canonical UUID values.
func validateSourceReadUUID(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("source read: %s is required", name)
	}
	u, err := util.ParseUUID(value)
	if err != nil || !u.Valid || u.Bytes == [16]byte{} {
		return fmt.Errorf("source read: %s must be a UUID", name)
	}
	if canonical := u.String(); canonical != value {
		return fmt.Errorf("source read: %s must be a canonical UUID", name)
	}
	return nil
}

// validateSourceReadOpaqueID rejects blank opaque identifiers. Daemon IDs are
// opaque server-issued strings, never UUID-validated.
func validateSourceReadOpaqueID(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("source read: %s is required", name)
	}
	return nil
}

// validateSourceReadToken enforces the capability shape at the client
// boundary: msr_ prefix, bearer type, source:read scope, exact echoed
// selectors, and a finite positive lifetime of at most 120 seconds.
func validateSourceReadToken(resp *sourceReadTokenResponse, runtimeID, workspaceID, daemonID string, sentAt, receivedAt time.Time) error {
	if err := requireSourceReadToken(resp.Token); err != nil {
		return fmt.Errorf("source read token: missing %s prefix", msrTokenPrefix)
	}
	if resp.TokenType != "Bearer" {
		return fmt.Errorf("source read token: token_type must be Bearer")
	}
	if resp.Scope != sourceReadScope {
		return fmt.Errorf("source read token: scope must be %s", sourceReadScope)
	}
	if resp.RuntimeID != runtimeID || resp.WorkspaceID != workspaceID || resp.DaemonID != daemonID {
		return fmt.Errorf("source read token: echoed selectors do not match request")
	}
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		return fmt.Errorf("source read token: expires_at must be RFC3339")
	}
	if resp.ExpiresIn <= 0 || resp.ExpiresIn > 120 {
		return fmt.Errorf("source read token: expires_in must be within 1..120")
	}
	lifetime := expiresAt.Sub(receivedAt)
	if lifetime <= 0 || lifetime > sourceReadMaxTTL {
		return fmt.Errorf("source read token: lifetime must be within %s", sourceReadMaxTTL)
	}
	// expires_in is computed when minted, not when the response arrives.
	// Allow network latency and whole-second expiry rounding, without
	// accepting a claimed mint time outside this request's time window.
	issuedAt := expiresAt.Add(-time.Duration(resp.ExpiresIn) * time.Second)
	if issuedAt.Before(sentAt.Add(-time.Second)) || issuedAt.After(receivedAt.Add(time.Second)) {
		return fmt.Errorf("source read token: expires_in disagrees with request window")
	}
	return nil
}

// parseSourceReadCommands validates one list response against the transport
// contract. Unknown commands, invalid statuses, malformed IDs, invalid
// revisions, blank handles, or malformed list fields are rejected wholesale.
func parseSourceReadCommands(items []sourceReadCommandDTO, workspaceID string) ([]SourceReadCommand, error) {
	out := make([]SourceReadCommand, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if seen[it.ID] {
			return nil, fmt.Errorf("source read: duplicate command id %s", it.ID)
		}
		seen[it.ID] = true
		cmd := SourceReadCommand{
			ID: it.ID, RequestID: it.RequestID, WorkspaceID: it.WorkspaceID,
			SourceID: it.SourceID, SourceHandle: it.SourceHandle, Command: it.Command,
			NativeID: it.NativeID, LimitCount: it.LimitCount, ConfigRevision: it.ConfigRev,
			ExpiresAt: it.ExpiresAt, Status: it.Status, ClaimedRuntimeID: it.ClaimedRunID,
			Result: it.Result, Error: it.Error,
		}
		if err := validateSourceReadIDs(map[string]string{
			"command id":   it.ID,
			"request id":   it.RequestID,
			"workspace id": it.WorkspaceID,
			"source id":    it.SourceID,
		}); err != nil {
			return nil, err
		}
		if it.WorkspaceID != workspaceID {
			return nil, fmt.Errorf("source read command %s: workspace %s does not match request", it.ID, it.WorkspaceID)
		}
		// The pending list is a work queue: anything not pending, or already
		// carrying claim/result/error state, is a contract violation.
		if it.Status != "pending" {
			return nil, fmt.Errorf("source read command %s: pending list must only contain pending commands, got %q", it.ID, it.Status)
		}
		if it.ClaimedRunID != "" || it.Result != "" || it.Error != "" {
			return nil, fmt.Errorf("source read command %s: pending list must not carry claim/result/error", it.ID)
		}
		if strings.TrimSpace(it.SourceHandle) == "" {
			return nil, fmt.Errorf("source read command %s: blank source handle", it.ID)
		}
		if err := validateSourceReadParameters(&it, true); err != nil {
			return nil, err
		}
		out = append(out, cmd)
	}
	return out, nil
}

// Terminal receipts retain their original deadline for idempotent report replay.
func validateSourceReadParameters(it *sourceReadCommandDTO, current bool) error {
	if it.ConfigRev <= 0 {
		return fmt.Errorf("source read: config_revision must be positive")
	}
	expiresAt, err := time.Parse(time.RFC3339, it.ExpiresAt)
	if err != nil || (current && !expiresAt.After(time.Now())) {
		return fmt.Errorf("source read: invalid command expiry")
	}
	switch it.Command {
	case "list":
		if it.NativeID != "" || it.LimitCount < 0 || it.LimitCount > 200 {
			return fmt.Errorf("source read: invalid list parameters")
		}
	case "read":
		if strings.TrimSpace(it.NativeID) == "" || it.LimitCount != 0 {
			return fmt.Errorf("source read: invalid read parameters")
		}
	default:
		return fmt.Errorf("source read: unknown command %q", it.Command)
	}
	return nil
}

// ExchangeSourceReadToken exchanges the client's normal human credential for a
// short-lived msr_ capability bound to one runtime. The credential is returned
// to the caller; the client's own token is never touched.
func (c *Client) ExchangeSourceReadToken(ctx context.Context, runtimeID, workspaceID, daemonID string) (SourceReadCredential, error) {
	if err := validateSourceReadIDs(map[string]string{
		"runtime id":   runtimeID,
		"workspace id": workspaceID,
	}); err != nil {
		return SourceReadCredential{}, err
	}
	if err := validateSourceReadOpaqueID("daemon id", daemonID); err != nil {
		return SourceReadCredential{}, err
	}
	var resp sourceReadTokenResponse
	path := fmt.Sprintf("/api/daemon/runtimes/%s/source-read-token", url.PathEscape(runtimeID))
	sentAt := time.Now()
	if err := c.postJSONWithToken(ctx, path, c.Token(), map[string]string{"scope": sourceReadScope}, &resp); err != nil {
		return SourceReadCredential{}, err
	}
	now := time.Now()
	if err := validateSourceReadToken(&resp, runtimeID, workspaceID, daemonID, sentAt, now); err != nil {
		return SourceReadCredential{}, err
	}
	expiresAt, _ := time.Parse(time.RFC3339, resp.ExpiresAt)
	return SourceReadCredential{
		Token: resp.Token, ExpiresAt: expiresAt, ExpiresIn: resp.ExpiresIn,
		RuntimeID: resp.RuntimeID, WorkspaceID: resp.WorkspaceID,
	}, nil
}

// requireSourceReadToken enforces the capability shape before a scoped call.
func requireSourceReadToken(token string) error {
	if !strings.HasPrefix(token, msrTokenPrefix) || strings.TrimSpace(strings.TrimPrefix(token, msrTokenPrefix)) == "" {
		return fmt.Errorf("source read: explicit %s credential required", msrTokenPrefix)
	}
	return nil
}

// sourceReadCommandResponse is the claim/receipt reply shape. SourceHandle is
// deliberately absent: the claim receipt omits it, so the dispatcher must keep
// the pending item's handle locally.
type sourceReadCommandResponse = sourceReadCommandDTO

// validateSourceReadReceipt checks a single-receipt response for the exact
// requested id/workspace and required fields.
func validateSourceReadReceipt(dto *sourceReadCommandResponse, wantWorkspace, wantStatus string) error {
	if err := validateSourceReadIDs(map[string]string{
		"command id":   dto.ID,
		"request id":   dto.RequestID,
		"workspace id": dto.WorkspaceID,
		"source id":    dto.SourceID,
	}); err != nil {
		return err
	}
	if dto.WorkspaceID != wantWorkspace {
		return fmt.Errorf("source read: receipt workspace %s does not match %s", dto.WorkspaceID, wantWorkspace)
	}
	if dto.Status == "" {
		return fmt.Errorf("source read: receipt status missing")
	}
	if wantStatus != "" && dto.Status != wantStatus {
		return fmt.Errorf("source read: receipt status %s does not match expected %s", dto.Status, wantStatus)
	}
	// Receipts are bounded at the sender side; a hostile or buggy server
	// must not push an oversized payload through the receipt either.
	if len(dto.Result) > sourceReadResultMaxBytes {
		return fmt.Errorf("source read: receipt result exceeds %d bytes", sourceReadResultMaxBytes)
	}
	if len(dto.Error) > sourceReadErrorMaxBytes {
		return fmt.Errorf("source read: receipt error exceeds %d bytes", sourceReadErrorMaxBytes)
	}
	return validateSourceReadParameters(dto, wantStatus == "claimed")
}

// ListSourceReadCommands lists pending read receipts for the exact runtime the
// capability is bound to, using the explicit msr_ token.
func (c *Client) ListSourceReadCommands(ctx context.Context, runtimeID, workspaceID, token string) ([]SourceReadCommand, error) {
	if err := validateSourceReadIDs(map[string]string{
		"runtime id":   runtimeID,
		"workspace id": workspaceID,
	}); err != nil {
		return nil, err
	}
	if err := requireSourceReadToken(token); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := fmt.Sprintf("/api/daemon/runtimes/%s/work-source-commands", url.PathEscape(runtimeID))
	if err := c.getJSONWithToken(ctx, path, token, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, fmt.Errorf("source read: list response must be an array")
	}
	var items []sourceReadCommandDTO
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("source read: list response must be an array")
	}
	return parseSourceReadCommands(items, workspaceID)
}

// ClaimSourceReadCommand transitions one pending receipt to claimed using the
// msr_ credential. The returned command carries the exact requested ID and
// workspace; the receipt has no SourceHandle, so the caller must retain the
// pending item's handle.
func (c *Client) ClaimSourceReadCommand(ctx context.Context, runtimeID, workspaceID, commandID, token string) (SourceReadCommand, error) {
	if err := validateSourceReadIDs(map[string]string{
		"runtime id":   runtimeID,
		"workspace id": workspaceID,
		"command id":   commandID,
	}); err != nil {
		return SourceReadCommand{}, err
	}
	if err := requireSourceReadToken(token); err != nil {
		return SourceReadCommand{}, err
	}
	var dto sourceReadCommandResponse
	path := fmt.Sprintf("/api/daemon/runtimes/%s/work-source-commands/%s/claim",
		url.PathEscape(runtimeID), url.PathEscape(commandID))
	if err := c.postJSONWithToken(ctx, path, token, map[string]string{}, &dto); err != nil {
		return SourceReadCommand{}, err
	}
	if dto.ID != commandID {
		return SourceReadCommand{}, fmt.Errorf("source read: claim receipt id %s does not match %s", dto.ID, commandID)
	}
	if err := validateSourceReadReceipt(&dto, workspaceID, "claimed"); err != nil {
		return SourceReadCommand{}, err
	}
	if dto.Result != "" || dto.Error != "" {
		return SourceReadCommand{}, fmt.Errorf("source read: claim carries terminal payload")
	}
	if dto.ClaimedRunID != runtimeID {
		return SourceReadCommand{}, fmt.Errorf("source read: claim receipt claimed_runtime %s does not match %s", dto.ClaimedRunID, runtimeID)
	}
	return SourceReadCommand{
		ID: dto.ID, RequestID: dto.RequestID, WorkspaceID: dto.WorkspaceID,
		SourceID: dto.SourceID, Command: dto.Command, NativeID: dto.NativeID,
		LimitCount: dto.LimitCount, ConfigRevision: dto.ConfigRev,
		ExpiresAt: dto.ExpiresAt, Status: dto.Status, ClaimedRuntimeID: dto.ClaimedRunID,
	}, nil
}

// ReportSourceReadCommand submits the terminal receipt for one command:
// succeeded with a JSON-encoded result string, or failed with a bounded
// diagnostic. The returned receipt matches the requested ID, workspace, and
// reported status.
func (c *Client) ReportSourceReadCommand(ctx context.Context, runtimeID, workspaceID, commandID, token, status, result, diagnostic string) (SourceReadCommand, error) {
	if err := validateSourceReadIDs(map[string]string{
		"runtime id":   runtimeID,
		"workspace id": workspaceID,
		"command id":   commandID,
	}); err != nil {
		return SourceReadCommand{}, err
	}
	if err := requireSourceReadToken(token); err != nil {
		return SourceReadCommand{}, err
	}
	var body struct {
		Status string `json:"status"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	switch status {
	case "succeeded":
		if strings.TrimSpace(result) == "" {
			return SourceReadCommand{}, fmt.Errorf("source read: result is required for succeeded")
		}
		if !json.Valid([]byte(result)) {
			return SourceReadCommand{}, fmt.Errorf("source read: result must be a JSON-encoded string")
		}
		if len(result) > sourceReadResultMaxBytes {
			return SourceReadCommand{}, fmt.Errorf("source read: result exceeds %d bytes", sourceReadResultMaxBytes)
		}
		if diagnostic != "" {
			return SourceReadCommand{}, fmt.Errorf("source read: error is not valid for succeeded")
		}
		body = struct {
			Status string `json:"status"`
			Result string `json:"result,omitempty"`
			Error  string `json:"error,omitempty"`
		}{Status: status, Result: result}
	case "failed":
		if len(diagnostic) > sourceReadErrorMaxBytes {
			return SourceReadCommand{}, fmt.Errorf("source read: error exceeds %d bytes", sourceReadErrorMaxBytes)
		}
		if result != "" {
			return SourceReadCommand{}, fmt.Errorf("source read: result is not valid for failed")
		}
		body = struct {
			Status string `json:"status"`
			Result string `json:"result,omitempty"`
			Error  string `json:"error,omitempty"`
		}{Status: status, Error: diagnostic}
	default:
		return SourceReadCommand{}, fmt.Errorf("source read: status must be succeeded or failed")
	}
	var dto sourceReadCommandResponse
	path := fmt.Sprintf("/api/daemon/runtimes/%s/work-source-commands/%s/result",
		url.PathEscape(runtimeID), url.PathEscape(commandID))
	if err := c.postJSONWithToken(ctx, path, token, body, &dto); err != nil {
		return SourceReadCommand{}, err
	}
	if dto.ID != commandID {
		return SourceReadCommand{}, fmt.Errorf("source read: report receipt id %s does not match %s", dto.ID, commandID)
	}
	if err := validateSourceReadReceipt(&dto, workspaceID, status); err != nil {
		return SourceReadCommand{}, err
	}
	if dto.ClaimedRunID != runtimeID || dto.Result != body.Result || dto.Error != body.Error {
		return SourceReadCommand{}, fmt.Errorf("source read: report receipt claimer or payload mismatch")
	}
	return SourceReadCommand{
		ID: dto.ID, RequestID: dto.RequestID, WorkspaceID: dto.WorkspaceID,
		SourceID: dto.SourceID, Command: dto.Command, NativeID: dto.NativeID,
		LimitCount: dto.LimitCount, ConfigRevision: dto.ConfigRev,
		ExpiresAt: dto.ExpiresAt, Status: dto.Status, ClaimedRuntimeID: dto.ClaimedRunID,
		Result: dto.Result, Error: dto.Error,
	}, nil
}
