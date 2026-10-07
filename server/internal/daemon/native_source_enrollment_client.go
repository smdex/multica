package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Native source-enrollment transport constants mirroring the server contract
// in server/internal/handler/native_source_enrollment.go,
// internal/auth/source_enrollment_token.go and work_source.go.
const (
	// mseTokenPrefix marks minted source-enrollment capabilities.
	mseTokenPrefix = "mse_"
	// nativeEnrollmentScope is the only scope the mint route returns.
	nativeEnrollmentScope = "source:enroll"
	// nativeEnrollmentMaxTTL bounds the capability lifetime at the client
	// boundary; the server mints 120-second capabilities.
	nativeEnrollmentMaxTTL = 120 * time.Second
	// nativeEnrollmentMaxBodyBytes bounds single-object responses (16 KiB).
	nativeEnrollmentMaxBodyBytes = 16 << 10
	// nativeEnrollmentListMaxBytes bounds the work-source list response (2 MiB).
	nativeEnrollmentListMaxBytes = 2 << 20
)

// NativeSourceEnrollment mirrors the workSourceResponse JSON the enrollment
// routes and the workspace work-source list return.
type NativeSourceEnrollment struct {
	ID                     string `json:"id"`
	WorkspaceID            string `json:"workspace_id"`
	RuntimeID              string `json:"runtime_id"`
	DaemonID               string `json:"daemon_id"`
	Name                   string `json:"name"`
	Mode                   string `json:"mode"`
	Enabled                bool   `json:"enabled"`
	SourceHandle           string `json:"source_handle"`
	ConfigRevision         int32  `json:"config_revision"`
	NativeEnrollmentID     string `json:"native_enrollment_id,omitempty"`
	NativeEnrollmentStatus string `json:"native_enrollment_status,omitempty"`
	NativeManifestHash     string `json:"native_manifest_hash,omitempty"`
	NativeOwnerMemberID    string `json:"native_owner_member_id,omitempty"`
	NativeRuntimeCreatedAt string `json:"native_runtime_created_at,omitempty"`
	NativeEnrolledAt       string `json:"native_enrolled_at,omitempty"`
}

// NativeSourceEnrollmentProof binds finalization and minting to one immutable
// enrollment marker. JSON tags match nativeEnrollmentProofRequest on the server.
type NativeSourceEnrollmentProof struct {
	EnrollmentID   string `json:"enrollment_id"`
	ConfigRevision int32  `json:"config_revision"`
	ManifestHash   string `json:"manifest_hash"`
}

// SourceEnrollmentCredential is a short-lived, runtime-scoped enrollment
// capability. It never replaces or mutates the client's human token; callers
// pass it explicitly to FinalizeNativeSourceEnrollment.
type SourceEnrollmentCredential struct {
	Token          string
	ExpiresAt      time.Time
	ExpiresIn      int64
	RuntimeID      string
	WorkspaceID    string
	DaemonID       string
	SourceID       string
	EnrollmentID   string
	ConfigRevision int32
	ManifestHash   string
}

// nativeEnrollmentTokenResponse is the mint route's JSON body.
type nativeEnrollmentTokenResponse struct {
	Token          string `json:"token"`
	TokenType      string `json:"token_type"`
	Scope          string `json:"scope"`
	WorkspaceID    string `json:"workspace_id"`
	RuntimeID      string `json:"runtime_id"`
	DaemonID       string `json:"daemon_id"`
	SourceID       string `json:"source_id"`
	EnrollmentID   string `json:"enrollment_id"`
	ConfigRevision int64  `json:"config_revision"`
	ManifestHash   string `json:"manifest_hash"`
	ExpiresAt      string `json:"expires_at"`
	ExpiresIn      int64  `json:"expires_in"`
}

// validateNativeManifestHash rejects anything but lowercase hex of exactly
// sha256 length.
func validateNativeManifestHash(value string) error {
	if len(value) != 64 {
		return fmt.Errorf("native enrollment: manifest hash must be 64 lowercase hex characters")
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("native enrollment: manifest hash must be 64 lowercase hex characters")
		}
	}
	return nil
}

// validateNativeEnrollmentProof checks the proof shape before any request.
func validateNativeEnrollmentProof(proof NativeSourceEnrollmentProof) error {
	if err := validateSourceReadUUID("enrollment id", proof.EnrollmentID); err != nil {
		return err
	}
	if proof.ConfigRevision <= 0 {
		return fmt.Errorf("native enrollment: config revision must be positive")
	}
	return validateNativeManifestHash(proof.ManifestHash)
}

// validateNativeEnrollmentSelectors checks the request identifiers shared by
// every native enrollment call.
func validateNativeEnrollmentSelectors(runtimeID, workspaceID, daemonID string) error {
	if err := validateSourceReadIDs(map[string]string{
		"runtime id":   runtimeID,
		"workspace id": workspaceID,
	}); err != nil {
		return err
	}
	return validateSourceReadOpaqueID("daemon id", daemonID)
}

// validateNativeEnrollmentSource validates one decoded work-source payload
// against the exact request selectors and the native enrollment invariants.
// status must be "pending" or "enrolled"; a pending enrollment is always
// disabled, an enrolled one may already be enabled on idempotent retry.
func validateNativeEnrollmentSource(s *NativeSourceEnrollment, runtimeID, workspaceID, daemonID string) error {
	if err := validateSourceReadIDs(map[string]string{
		"source id":    s.ID,
		"workspace id": s.WorkspaceID,
		"runtime id":   s.RuntimeID,
	}); err != nil {
		return err
	}
	if s.WorkspaceID != workspaceID || s.RuntimeID != runtimeID || s.DaemonID != daemonID {
		return fmt.Errorf("native enrollment: echoed selectors do not match request")
	}
	if s.Mode != "native" {
		return fmt.Errorf("native enrollment: mode must be native")
	}
	if s.SourceHandle != "managed:"+s.ID {
		return fmt.Errorf("native enrollment: source handle must be managed")
	}
	if s.ConfigRevision <= 0 {
		return fmt.Errorf("native enrollment: config revision must be positive")
	}
	if err := validateSourceReadUUID("enrollment id", s.NativeEnrollmentID); err != nil {
		return err
	}
	if err := validateSourceReadUUID("owner member id", s.NativeOwnerMemberID); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, s.NativeRuntimeCreatedAt); err != nil {
		return fmt.Errorf("native enrollment: runtime created_at must be RFC3339")
	}
	switch s.NativeEnrollmentStatus {
	case "pending":
		if s.Enabled {
			return fmt.Errorf("native enrollment: pending enrollment must be disabled")
		}
		if s.NativeEnrolledAt != "" {
			return fmt.Errorf("native enrollment: pending enrollment must not carry enrolled_at")
		}
		if s.NativeManifestHash != "" {
			if err := validateNativeManifestHash(s.NativeManifestHash); err != nil {
				return err
			}
		}
	case "enrolled":
		if err := validateNativeManifestHash(s.NativeManifestHash); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339, s.NativeEnrolledAt); err != nil {
			return fmt.Errorf("native enrollment: enrolled_at must be RFC3339")
		}
	default:
		return fmt.Errorf("native enrollment: unknown enrollment status")
	}
	return nil
}

// requireMSEToken enforces the explicit capability shape before a scoped call.
// Unlike requireSourceReadToken it also rejects embedded whitespace or control
// characters, which must never appear in a bearer header value.
func requireMSEToken(token string) error {
	if !strings.HasPrefix(token, mseTokenPrefix) || strings.TrimSpace(strings.TrimPrefix(token, mseTokenPrefix)) == "" {
		return fmt.Errorf("native enrollment: explicit %s credential required", mseTokenPrefix)
	}
	for _, r := range token {
		if r <= 0x20 || r > 0x7e {
			return fmt.Errorf("native enrollment: credential contains invalid characters")
		}
	}
	return nil
}

// validateNativeEnrollmentToken enforces the minted capability shape: mse_
// prefix without whitespace/control characters, bearer type, source:enroll
// scope, exact echoed selectors, proof agreement, and a bounded future
// lifetime of at most 120 seconds.
func validateNativeEnrollmentToken(resp *nativeEnrollmentTokenResponse, runtimeID, workspaceID, daemonID, sourceID string, proof NativeSourceEnrollmentProof, sentAt, receivedAt time.Time) error {
	if err := requireMSEToken(resp.Token); err != nil {
		return err
	}
	if resp.TokenType != "Bearer" {
		return fmt.Errorf("native enrollment: token_type must be Bearer")
	}
	if resp.Scope != nativeEnrollmentScope {
		return fmt.Errorf("native enrollment: scope must be %s", nativeEnrollmentScope)
	}
	if resp.RuntimeID != runtimeID || resp.WorkspaceID != workspaceID || resp.DaemonID != daemonID || resp.SourceID != sourceID {
		return fmt.Errorf("native enrollment: echoed selectors do not match request")
	}
	if resp.EnrollmentID != proof.EnrollmentID || resp.ConfigRevision != int64(proof.ConfigRevision) || resp.ManifestHash != proof.ManifestHash {
		return fmt.Errorf("native enrollment: capability does not match proof")
	}
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		return fmt.Errorf("native enrollment: expires_at must be RFC3339")
	}
	if resp.ExpiresIn <= 0 || resp.ExpiresIn > 120 {
		return fmt.Errorf("native enrollment: expires_in must be within 1..120")
	}
	if !expiresAt.After(receivedAt) || expiresAt.Sub(receivedAt) > nativeEnrollmentMaxTTL {
		return fmt.Errorf("native enrollment: lifetime must be within %s", nativeEnrollmentMaxTTL)
	}
	issuedAt := expiresAt.Add(-time.Duration(resp.ExpiresIn) * time.Second)
	if issuedAt.Before(sentAt.Add(-time.Second)) || issuedAt.After(receivedAt.Add(time.Second)) {
		return fmt.Errorf("native enrollment: expires_in disagrees with request window")
	}
	return nil
}

// nativeEnrollmentExchange is the single narrow transport for native
// enrollment calls. Unlike postJSONWithToken it always sends an exact
// X-Workspace-ID header, enforces controlled response statuses, caps the
// response body, and rejects anything but a single JSON document (json.Unmarshal
// fails on trailing content). Failure errors are fixed and never carry the
// response body or any credential material.
func (c *Client) nativeEnrollmentExchange(ctx context.Context, method, path, token, workspaceID string, reqBody any, respBody any, maxBytes int64, allowedStatuses map[int]bool) (int, error) {
	var body io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return 0, err
		}
		body = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", workspaceID)
	c.setIdentityHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return 0, err
	}
	if !allowedStatuses[resp.StatusCode] {
		// Deliberately no response body: server error bodies are untrusted at
		// this boundary and may echo request material.
		return 0, &requestError{Method: method, Path: path, StatusCode: resp.StatusCode}
	}
	if int64(len(data)) > maxBytes {
		return 0, fmt.Errorf("native enrollment: response exceeds %d bytes", maxBytes)
	}
	if respBody == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(data, respBody); err != nil {
		return 0, fmt.Errorf("native enrollment: invalid response body")
	}
	return resp.StatusCode, nil
}

// nativeEnrollmentPostIssues is the controlled success set shared by all
// single-object POSTs here; finalize and mint only ever answer 200, create
// answers 200 (replay) or 201 (new).
var nativeEnrollmentStatusOK = map[int]bool{http.StatusOK: true}

// CreateNativeSourceIntent registers the intent to enroll one native source on
// the given runtime, using the client's human credential. A replayed request
// returns the same source with 200; a new intent returns 201.
func (c *Client) CreateNativeSourceIntent(ctx context.Context, runtimeID, workspaceID, daemonID, requestID, name string) (NativeSourceEnrollment, error) {
	if err := validateNativeEnrollmentSelectors(runtimeID, workspaceID, daemonID); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if err := validateSourceReadUUID("request id", requestID); err != nil {
		return NativeSourceEnrollment{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return NativeSourceEnrollment{}, fmt.Errorf("native enrollment: name must be 1..200 characters")
	}
	var out NativeSourceEnrollment
	path := fmt.Sprintf("/api/daemon/runtimes/%s/source-enrollments", url.PathEscape(runtimeID))
	body := struct {
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
	}{RequestID: requestID, Name: name}
	status, err := c.nativeEnrollmentExchange(ctx, http.MethodPost, path, c.Token(), workspaceID, body, &out,
		nativeEnrollmentMaxBodyBytes, map[int]bool{http.StatusOK: true, http.StatusCreated: true})
	if err != nil {
		return NativeSourceEnrollment{}, err
	}
	if err := validateNativeEnrollmentSource(&out, runtimeID, workspaceID, daemonID); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if status == http.StatusCreated && out.NativeEnrollmentStatus != "pending" {
		return NativeSourceEnrollment{}, fmt.Errorf("native enrollment: created intent must be pending")
	}
	return out, nil
}

// MintSourceEnrollmentToken exchanges the client's human credential for a
// short-lived mse_ capability bound to one source and proof. The credential is
// returned to the caller; the client's own token is never touched.
func (c *Client) MintSourceEnrollmentToken(ctx context.Context, runtimeID, workspaceID, daemonID, sourceID string, proof NativeSourceEnrollmentProof) (SourceEnrollmentCredential, error) {
	if err := validateNativeEnrollmentSelectors(runtimeID, workspaceID, daemonID); err != nil {
		return SourceEnrollmentCredential{}, err
	}
	if err := validateSourceReadUUID("source id", sourceID); err != nil {
		return SourceEnrollmentCredential{}, err
	}
	if err := validateNativeEnrollmentProof(proof); err != nil {
		return SourceEnrollmentCredential{}, err
	}
	var resp nativeEnrollmentTokenResponse
	path := fmt.Sprintf("/api/daemon/runtimes/%s/source-enrollments/%s/token", url.PathEscape(runtimeID), url.PathEscape(sourceID))
	sentAt := time.Now()
	if _, err := c.nativeEnrollmentExchange(ctx, http.MethodPost, path, c.Token(), workspaceID, proof, &resp,
		nativeEnrollmentMaxBodyBytes, nativeEnrollmentStatusOK); err != nil {
		return SourceEnrollmentCredential{}, err
	}
	receivedAt := time.Now()
	if err := validateNativeEnrollmentToken(&resp, runtimeID, workspaceID, daemonID, sourceID, proof, sentAt, receivedAt); err != nil {
		return SourceEnrollmentCredential{}, err
	}
	expiresAt, _ := time.Parse(time.RFC3339, resp.ExpiresAt)
	return SourceEnrollmentCredential{
		Token: resp.Token, ExpiresAt: expiresAt, ExpiresIn: resp.ExpiresIn,
		RuntimeID: resp.RuntimeID, WorkspaceID: resp.WorkspaceID, DaemonID: resp.DaemonID,
		SourceID: resp.SourceID, EnrollmentID: resp.EnrollmentID,
		ConfigRevision: int32(resp.ConfigRevision), ManifestHash: resp.ManifestHash,
	}, nil
}

// FinalizeNativeSourceEnrollment completes the enrollment using only the
// explicit mse_ capability minted for this source and proof. It never reads or
// mutates the client's human token.
func (c *Client) FinalizeNativeSourceEnrollment(ctx context.Context, runtimeID, workspaceID, daemonID, sourceID, token string, proof NativeSourceEnrollmentProof) (NativeSourceEnrollment, error) {
	if err := validateNativeEnrollmentSelectors(runtimeID, workspaceID, daemonID); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if err := validateSourceReadUUID("source id", sourceID); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if err := validateNativeEnrollmentProof(proof); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if err := requireMSEToken(token); err != nil {
		return NativeSourceEnrollment{}, err
	}
	var out NativeSourceEnrollment
	path := fmt.Sprintf("/api/daemon/runtimes/%s/source-enrollments/%s/finalize", url.PathEscape(runtimeID), url.PathEscape(sourceID))
	if _, err := c.nativeEnrollmentExchange(ctx, http.MethodPost, path, token, workspaceID, proof, &out,
		nativeEnrollmentMaxBodyBytes, nativeEnrollmentStatusOK); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if err := validateNativeEnrollmentSource(&out, runtimeID, workspaceID, daemonID); err != nil {
		return NativeSourceEnrollment{}, err
	}
	if out.NativeEnrollmentStatus != "enrolled" {
		return NativeSourceEnrollment{}, fmt.Errorf("native enrollment: finalized source must be enrolled")
	}
	if out.ID != sourceID || out.NativeEnrollmentID != proof.EnrollmentID || out.ConfigRevision != proof.ConfigRevision || out.NativeManifestHash != proof.ManifestHash {
		return NativeSourceEnrollment{}, fmt.Errorf("native enrollment: finalized source does not match request")
	}
	return out, nil
}

// GetNativeSourceEnrollment fetches one source by exact UUID selector through
// the workspace work-source list, using the client's human credential.
func (c *Client) GetNativeSourceEnrollment(ctx context.Context, workspaceID, sourceID string) (NativeSourceEnrollment, error) {
	if err := validateSourceReadIDs(map[string]string{
		"workspace id": workspaceID,
		"source id":    sourceID,
	}); err != nil {
		return NativeSourceEnrollment{}, err
	}
	var items []NativeSourceEnrollment
	if _, err := c.nativeEnrollmentExchange(ctx, http.MethodGet, "/api/work-sources", c.Token(), workspaceID, nil, &items,
		nativeEnrollmentListMaxBytes, nativeEnrollmentStatusOK); err != nil {
		return NativeSourceEnrollment{}, err
	}
	for i := range items {
		if items[i].ID != sourceID {
			continue
		}
		if err := validateNativeEnrollmentSource(&items[i], items[i].RuntimeID, workspaceID, items[i].DaemonID); err != nil {
			return NativeSourceEnrollment{}, err
		}
		return items[i], nil
	}
	return NativeSourceEnrollment{}, fmt.Errorf("native enrollment: source not found in workspace")
}
