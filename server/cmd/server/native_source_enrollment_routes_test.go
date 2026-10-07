package main

// Production-router regression CONTRACT for the dedicated native source
// enrollment endpoints, specified by the coordinator's architecture handoff
// (see source_read_token_routes_test.go for the shipped pattern these build
// on):
//
//	POST /api/daemon/runtimes/{runtimeId}/source-enrollments
//	     {request_id,name} — fresh human JWT/PAT only
//	POST /api/daemon/runtimes/{runtimeId}/source-enrollments/{sourceId}/token
//	     {enrollment_id,config_revision,manifest_hash} — fresh human JWT/PAT
//	POST /api/daemon/runtimes/{runtimeId}/source-enrollments/{sourceId}/finalize
//	     same proof body — Bearer mse_ ONLY (scope fenced to this exact path)
//
// These tests are the acceptance contract for the (not yet wired) root-owned
// handlers, middleware, and SQL. Until that lands, they cannot compile beyond
// vet and are expected to FAIL once runnable: they encode required behavior,
// not current behavior. Root may adjust the response shapes here after
// implementation.
//
// Policy under test (MVP authority: the same operator must be BOTH current
// workspace owner/admin AND exact non-NULL runtime owner):
//   - intent: 201 with pending native source, disabled, managed handle;
//     identical replay 200 same S/E; conflicting request_id inputs 409;
//     member-not-admin / admin-not-runtime-owner / NULL owner 403;
//     wrong workspace 404; strict bodies 400.
//   - issuance: 200 with mse_-prefixed token, Bearer/source:enroll, exact
//     selectors, <=120s, no-store; revoking the parent PAT kills the child.
//   - finalize: mse-only; human/msr_/mdt_ (any valid non-mse daemon
//     credential) 403 on finalize and mse is 403 on every other daemon
//     route / 401 on user routes; enrolled receipt replays 200 only with a
//     still-valid capability; source delete removes the row (local files are
//     a client concern) and replayed finalize is 404.
//   - Generic Enable on a pending native source is 409 even for admin;
//     observe sources are never upgraded by these routes.
//   - No task/Issue/agent launches anywhere in this suite.
//
// Credential exchange failure output is sanitized: no decoded structs and no
// raw bodies from /token or /finalize responses are ever printed.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// nativeEnrollFixture stages an owned online runtime WITHOUT any work source:
// the intent endpoint itself creates the source.
func nativeEnrollFixture(t *testing.T, fx *testutil.Fixture, ownerID, name, provider string) string {
	t.Helper()
	daemonID := "mse-" + uuid.NewString()
	cols := testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         name, "daemon_id": daemonID, "provider": provider,
		"runtime_mode": "local", "status": "online", "visibility": "private",
		"device_info": "", "metadata": testutil.Raw("'{}'::jsonb"),
	}
	if ownerID != "" {
		cols["owner_id"] = ownerID
	} // else: ownerless runtime, insert NULL rather than an invalid UUID.
	return fx.Insert(t, "agent_runtime", cols)
}

// nativeEnrollIntent creates a pending native enrollment through the real
// router and returns (sourceID, enrollmentID).
func nativeEnrollIntent(t *testing.T, ctx context.Context, runtimeID, token, requestID, name string) (string, string) {
	t.Helper()
	_, data := mustSourceReadCall(t, ctx, http.MethodPost,
		"/api/daemon/runtimes/"+runtimeID+"/source-enrollments", token, testWorkspaceID,
		`{"request_id":"`+requestID+`","name":"`+name+`"}`, http.StatusCreated)
	var intent struct {
		ID                    string  `json:"id"`
		RuntimeID             string  `json:"runtime_id"`
		DaemonID              string  `json:"daemon_id"`
		WorkspaceID           string  `json:"workspace_id"`
		Mode                  string  `json:"mode"`
		Enabled               bool    `json:"enabled"`
		ConfigRevision        int32   `json:"config_revision"`
		SourceHandle          string  `json:"source_handle"`
		NativeEnrollmentID    string  `json:"native_enrollment_id"`
		NativeEnrollmentStat  string  `json:"native_enrollment_status"`
		NativeManifestHash    *string `json:"native_manifest_hash"`
		NativeOwnerMemberID   string  `json:"native_owner_member_id"`
		NativeRuntimeCreatedA string  `json:"native_runtime_created_at"`
		NativeEnrolledAt      *string `json:"native_enrolled_at"`
	}
	if err := json.Unmarshal(data, &intent); err != nil {
		t.Fatalf("intent response is not valid JSON: %v", err)
	}
	if intent.ID == "" || intent.RuntimeID != runtimeID || intent.WorkspaceID != testWorkspaceID {
		t.Fatalf("intent lost source/runtime/workspace identity: id_present=%t runtime_ok=%t workspace_ok=%t",
			intent.ID != "", intent.RuntimeID == runtimeID, intent.WorkspaceID == testWorkspaceID)
	}
	if intent.DaemonID == "" || intent.SourceHandle == "" {
		t.Fatalf("intent must derive daemon identity and a managed handle: daemon_present=%t handle_present=%t",
			intent.DaemonID != "", intent.SourceHandle != "")
	}
	if intent.Mode != "native" || intent.Enabled {
		t.Fatalf("intent must create a disabled native source: mode=%q enabled=%t", intent.Mode, intent.Enabled)
	}
	if intent.NativeEnrollmentID == "" || intent.NativeEnrollmentStat != "pending" {
		t.Fatalf("intent must carry server-generated enrollment id and pending status: id_present=%t status=%q",
			intent.NativeEnrollmentID != "", intent.NativeEnrollmentStat)
	}
	if intent.NativeManifestHash != nil {
		t.Fatal("manifest hash must be absent while pending")
	}
	if intent.NativeOwnerMemberID == "" || intent.NativeRuntimeCreatedA == "" {
		t.Fatalf("intent must pin owner member and runtime incarnation: member_present=%t runtime_created_at_present=%t",
			intent.NativeOwnerMemberID != "", intent.NativeRuntimeCreatedA != "")
	}
	if intent.NativeEnrolledAt != nil {
		t.Fatal("enrolled_at must be absent while pending")
	}
	return intent.ID, intent.NativeEnrollmentID
}

// nativeEnrollToken exchanges the approval proof for an mse_ capability.
// Body handling mirrors sourceReadExchange: minted credentials never enter
// failure logs.
func nativeEnrollToken(t *testing.T, ctx context.Context, runtimeID, sourceID, token, enrollmentID string, revision int32, manifestHash string) string {
	t.Helper()
	resp, data := mustNativeEnrollCall(t, ctx, http.MethodPost,
		"/api/daemon/runtimes/"+runtimeID+"/source-enrollments/"+sourceID+"/token",
		token, testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision, manifestHash), http.StatusOK)
	var issued struct {
		Token         string `json:"token"`
		TokenType     string `json:"token_type"`
		Scope         string `json:"scope"`
		RuntimeID     string `json:"runtime_id"`
		WorkspaceID   string `json:"workspace_id"`
		DaemonID      string `json:"daemon_id"`
		SourceID      string `json:"source_id"`
		EnrollmentID  string `json:"enrollment_id"`
		ConfigRevisio int64  `json:"config_revision"`
		ManifestHash  string `json:"manifest_hash"`
		ExpiresAt     string `json:"expires_at"`
		ExpiresIn     int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &issued); err != nil {
		t.Fatalf("invalid issuance response: err=%v", err)
	}
	if len(issued.Token) < len(auth.SourceEnrollmentTokenPrefix)+20 || !strings.HasPrefix(issued.Token, auth.SourceEnrollmentTokenPrefix) {
		t.Fatalf("token must be %s-prefixed", auth.SourceEnrollmentTokenPrefix)
	}
	if issued.TokenType != "Bearer" || issued.Scope != auth.SourceEnrollmentTokenPurpose {
		// Safe selectors only; never the struct, which includes the token.
		t.Fatalf("token_type/scope wrong: token_type=%q scope=%q", issued.TokenType, issued.Scope)
	}
	if issued.RuntimeID != runtimeID || issued.WorkspaceID != testWorkspaceID || issued.SourceID != sourceID ||
		issued.EnrollmentID != enrollmentID || issued.ManifestHash != manifestHash || issued.ConfigRevisio != int64(revision) {
		// Safe selector equality only; never the struct, which includes the token.
		t.Fatalf("issuance lost selector identity: runtime=%t workspace=%t source=%t enrollment=%t hash=%t revision=%t",
			issued.RuntimeID == runtimeID, issued.WorkspaceID == testWorkspaceID, issued.SourceID == sourceID,
			issued.EnrollmentID == enrollmentID, issued.ManifestHash == manifestHash, issued.ConfigRevisio == int64(revision))
	}
	if issued.DaemonID == "" {
		t.Fatal("issuance must carry the derived daemon id")
	}
	if issued.ExpiresIn <= 0 || issued.ExpiresIn > 120 {
		t.Fatalf("expires_in must be in (0,120]: %d", issued.ExpiresIn)
	}
	if expiresAt, err := time.Parse(time.RFC3339, issued.ExpiresAt); err != nil || time.Until(expiresAt) <= 0 || time.Until(expiresAt) > 121*time.Second {
		t.Fatalf("expires_at must be within 120s: err=%v", err)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control must be no-store, got %q", cc)
	}
	return issued.Token
}

func fmtInt(n int32) string {
	return strconv.Itoa(int(n))
}

// mustNativeEnrollCall wraps mustSourceReadCall with the same credential
// sanitization for the enrollment surfaces: any response from these routes
// may carry a minted mse_, so failure output prints status only.
func mustNativeEnrollCall(t *testing.T, ctx context.Context, method, path, token, workspace, body string, status int) (*http.Response, []byte) {
	t.Helper()
	resp, data, err := sourceReadCall(ctx, method, path, token, workspace, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d (credential exchange body omitted)", method, path, resp.StatusCode, status)
	}
	return resp, data
}

func nativeEnrollFinalizePath(runtimeID, sourceID string) string {
	return "/api/daemon/runtimes/" + runtimeID + "/source-enrollments/" + sourceID + "/finalize"
}

func nativeEnrollProofBody(enrollmentID string, revision int32, manifestHash string) string {
	return `{"enrollment_id":"` + enrollmentID +
		`","config_revision":` + fmtInt(revision) +
		`,"manifest_hash":"` + manifestHash + `"}`
}

// TestNativeSourceEnrollmentIntentAuthority pins the intent endpoint: MVP
// requires the same operator to be current workspace owner/admin AND exact
// non-NULL runtime owner; replay is idempotent; conflicting request_id reuse
// is 409; bodies are strict.
func TestNativeSourceEnrollmentIntentAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	outsider := fx.User(t, "mse outsider", "mse-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, outsider, "member")
	outsiderJWT, err := generateTestJWT(outsider, "", "mse outsider")
	if err != nil {
		t.Fatal(err)
	}
	admin := fx.User(t, "mse admin", "mse-admin-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, admin, "admin")
	adminJWT, err := generateTestJWT(admin, "", "mse admin")
	if err != nil {
		t.Fatal(err)
	}
	ownerPAT := sourceReadPAT(t, fx, testUserID)

	ownedRuntime := nativeEnrollFixture(t, fx, testUserID, "mse owner runtime", "mse-routes-owner")
	nullOwner := nativeEnrollFixture(t, fx, "", "mse null owner runtime", "mse-routes-null")
	intentPath := func(runtimeID string) string {
		return "/api/daemon/runtimes/" + runtimeID + "/source-enrollments"
	}
	body := func(requestID, name string) string {
		return `{"request_id":"` + requestID + `","name":"` + name + `"}`
	}

	// Anonymous is 401; plain member, admin-not-runtime-owner, and NULL-owner
	// runtimes are 403; wrong workspace header conceals as 404; malformed
	// runtime UUID is 400; malformed bodies are 400.
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), "", testWorkspaceID, body(uuid.NewString(), "x"), http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), outsiderJWT, testWorkspaceID, body(uuid.NewString(), "x"), http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), sourceReadPAT(t, fx, outsider), testWorkspaceID, body(uuid.NewString(), "x"), http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), adminJWT, testWorkspaceID, body(uuid.NewString(), "x"), http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(nullOwner), testToken, testWorkspaceID, body(uuid.NewString(), "x"), http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), testToken, uuid.NewString(), body(uuid.NewString(), "x"), http.StatusNotFound)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/runtimes/not-a-uuid/source-enrollments", testToken, testWorkspaceID, body(uuid.NewString(), "x"), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), testToken, testWorkspaceID, `{"request_id":"`+uuid.NewString()+`"}`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), testToken, testWorkspaceID, body(uuid.NewString(), "x")+`,`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), testToken, testWorkspaceID, `[{"request_id":"`+uuid.NewString()+`","name":"x"}]`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), testToken, testWorkspaceID, ``, http.StatusBadRequest)

	// Owner success over both real credential kinds, then replay.
	requestID := uuid.NewString()
	sourceID, enrollmentID := nativeEnrollIntent(t, ctx, ownedRuntime, testToken, requestID, "native docs")
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), ownerPAT, testWorkspaceID, body(requestID, "native docs"), http.StatusOK)
	// Same request_id, different name: conflicting immutable inputs are 409.
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(ownedRuntime), testToken, testWorkspaceID, body(requestID, "native docs renamed"), http.StatusConflict)
	// A different runtime reusing Q under the same workspace is also a
	// conflicting request (creator/runtime/name hash): 409, never a second E.
	mustSourceReadCall(t, ctx, http.MethodPost, intentPath(nativeEnrollFixture(t, fx, testUserID, "mse conflict runtime", "mse-routes-conflict")), testToken, testWorkspaceID, body(requestID, "native docs"), http.StatusConflict)

	// The pending source is visible as pending and NOT generically enabled.
	var status struct {
		Mode                string  `json:"mode"`
		Enabled             bool    `json:"enabled"`
		NativeEnrollmentSta string  `json:"native_enrollment_status"`
		NativeManifestHash  *string `json:"native_manifest_hash"`
	}
	_, listed := mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-sources", testToken, testWorkspaceID, "", http.StatusOK)
	var rows []json.RawMessage
	if err := json.Unmarshal(listed, &rows); err != nil {
		t.Fatalf("work-source list decode: %v", err)
	}
	found := false
	for _, row := range rows {
		var s struct {
			ID                  string  `json:"id"`
			Mode                string  `json:"mode"`
			Enabled             bool    `json:"enabled"`
			NativeEnrollmentID  string  `json:"native_enrollment_id"`
			NativeEnrollmentSta string  `json:"native_enrollment_status"`
			NativeManifestHash  *string `json:"native_manifest_hash"`
		}
		if json.Unmarshal(row, &s) != nil || s.ID != sourceID {
			continue
		}
		found = true
		status.Mode, status.Enabled = s.Mode, s.Enabled
		status.NativeEnrollmentSta, status.NativeManifestHash = s.NativeEnrollmentSta, s.NativeManifestHash
		if s.NativeEnrollmentID != enrollmentID {
			t.Fatalf("listing lost enrollment id: want E present=%t", s.NativeEnrollmentID != "")
		}
	}
	if !found {
		t.Fatalf("pending native source %s missing from listing", sourceID)
	}
	if status.Mode != "native" || status.Enabled || status.NativeEnrollmentSta != "pending" || status.NativeManifestHash != nil {
		t.Fatalf("listed pending native source wrong: mode=%q enabled=%t status=%q hash_present=%t",
			status.Mode, status.Enabled, status.NativeEnrollmentSta, status.NativeManifestHash != nil)
	}

	// Generic Enable on a pending native source is rejected even for the
	// owner/admin.
	mustSourceReadCall(t, ctx, http.MethodPatch, "/api/work-sources/"+sourceID, testToken, testWorkspaceID, `{"name":"native docs","enabled":true}`, http.StatusConflict)

	// An existing observe source is untouched by these routes: it keeps
	// observe mode and its own identity.
	observeRuntime := nativeEnrollFixture(t, fx, testUserID, "mse observe runtime", "mse-routes-observe")
	fx.Insert(t, "work_source", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"runtime_id": observeRuntime, "daemon_id": "mse-" + uuid.NewString(), "name": "mse observe",
		"source_handle": "mse-observe-" + uuid.NewString(), "mode": "observe",
	})
	var observeCount int
	fx.QueryRow(t, `SELECT count(*) FROM work_source WHERE runtime_id=$1 AND mode='observe'`, observeRuntime).Scan(&observeCount)
	if observeCount != 1 {
		t.Fatalf("observe sources must remain unchanged, got %d", observeCount)
	}
}

// TestNativeSourceEnrollmentTokenIssuance pins issuance authority, the mse_
// credential shape, strict proof bodies, and parent-PAT revocation.
func TestNativeSourceEnrollmentIssuanceAndRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	admin := fx.User(t, "mse issuance admin", "mse-issue-admin-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, admin, "admin")
	adminJWT, err := generateTestJWT(admin, "", "mse issuance admin")
	if err != nil {
		t.Fatal(err)
	}

	runtimeID := nativeEnrollFixture(t, fx, testUserID, "mse issuance runtime", "mse-routes-issue")
	sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, testToken, uuid.NewString(), "native issuance")
	tokenPath := "/api/daemon/runtimes/" + runtimeID + "/source-enrollments/" + sourceID + "/token"
	revision := int32(1)
	hash := strings.Repeat("a1", 32)
	proof := nativeEnrollProofBody(enrollmentID, revision, hash)

	// Issuance needs the same dual authority as intent.
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, "", testWorkspaceID, proof, http.StatusUnauthorized)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, adminJWT, testWorkspaceID, proof, http.StatusForbidden)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, uuid.NewString(), proof, http.StatusNotFound)

	// Strict bodies: unknown fields, wrong proof shape, trailing data.
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, testWorkspaceID, `{"enrollment_id":"`+enrollmentID+`"}`, http.StatusBadRequest)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, testWorkspaceID, proof+` {}`, http.StatusBadRequest)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, testWorkspaceID, `{"enrollment_id":"`+enrollmentID+`","config_revision":1,"manifest_hash":"`+hash+`","extra":1}`, http.StatusBadRequest)

	// Wrong proof values never mint.
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, testWorkspaceID, nativeEnrollProofBody(uuid.NewString(), revision, hash), http.StatusConflict)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision+1, hash), http.StatusConflict)

	mse := nativeEnrollToken(t, ctx, runtimeID, sourceID, testToken, enrollmentID, revision, hash)
	// Issuing twice pins H: a different hash for the same E is refused.
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, testToken, testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision, strings.Repeat("c3", 32)), http.StatusConflict)

	// mse is scope-fenced: rejected (403) on other daemon routes, 401 on
	// user routes, and cannot mint another capability.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/heartbeat", mse, testWorkspaceID, `{}`, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/source-read-token", mse, testWorkspaceID, `{"scope":"source:read"}`, http.StatusForbidden)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, mse, testWorkspaceID, proof, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/work-sources", mse, testWorkspaceID, `{"runtime_id":"`+runtimeID+`","name":"x","source_handle":"h"}`, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-sources", mse, testWorkspaceID, "", http.StatusUnauthorized)

	// Parent PAT revocation kills the child capability and re-issuance.
	member := fx.User(t, "mse revocable owner", "mse-revoke-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, member, "admin")
	pat := sourceReadPAT(t, fx, member)
	patRuntime := nativeEnrollFixture(t, fx, member, "mse revocation runtime", "mse-routes-revoke")
	patSource, patEnrollment := nativeEnrollIntent(t, ctx, patRuntime, pat, uuid.NewString(), "native revoke")
	patMSE := nativeEnrollToken(t, ctx, patRuntime, patSource, pat, patEnrollment, revision, hash)
	fx.Exec(t, `UPDATE personal_access_token SET revoked=TRUE WHERE token_hash=$1`, auth.HashToken(pat))
	mustSourceReadCall(t, ctx, http.MethodPost,
		"/api/daemon/runtimes/"+patRuntime+"/source-enrollments/"+patSource+"/token", pat, testWorkspaceID,
		nativeEnrollProofBody(patEnrollment, revision, hash), http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodPost, nativeEnrollFinalizePath(patRuntime, patSource), patMSE, testWorkspaceID,
		nativeEnrollProofBody(patEnrollment, revision, hash), http.StatusUnauthorized)
}

// TestNativeSourceEnrollmentFinalize pins the capability-only finalize route:
// only an mse_ on the exact method/runtime/source path commits enrolled;
// exact replay with a still-valid capability returns the same receipt;
// every other credential kind is denied; deletion removes the row.
func TestNativeSourceEnrollmentFinalize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	runtimeID := nativeEnrollFixture(t, fx, testUserID, "mse finalize runtime", "mse-routes-final")
	sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, testToken, uuid.NewString(), "native finalize")
	revision := int32(1)
	hash := strings.Repeat("d4", 32)
	proof := nativeEnrollProofBody(enrollmentID, revision, hash)
	finalizePath := nativeEnrollFinalizePath(runtimeID, sourceID)

	// No credential, human JWT/PAT, and a minted msr_ (valid token, wrong
	// capability) can never finalize.
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, "", testWorkspaceID, proof, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, testToken, testWorkspaceID, proof, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, sourceReadPAT(t, fx, testUserID), testWorkspaceID, proof, http.StatusForbidden)
	// An msr_ minted for THIS runtime is still the wrong capability surface.
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, msr, testWorkspaceID, proof, http.StatusForbidden)
	// Strict proof bodies are tested with the required capability, not a human.
	mse := nativeEnrollToken(t, ctx, runtimeID, sourceID, testToken, enrollmentID, revision, hash)
	mustNativeEnrollCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, `{"enrollment_id":"`+enrollmentID+`"}`, http.StatusBadRequest)

	// Wrong proof values under a valid capability are 409, not a commit.
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, nativeEnrollProofBody(uuid.NewString(), revision, hash), http.StatusConflict)
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision, strings.Repeat("e5", 32)), http.StatusConflict)

	// Commit: enrolled receipt with the pinned hash and timestamps.
	_, data := mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, proof, http.StatusOK)
	var receipt struct {
		ID                   string  `json:"id"`
		NativeEnrollmentID   string  `json:"native_enrollment_id"`
		NativeEnrollmentStat string  `json:"native_enrollment_status"`
		NativeManifestHash   *string `json:"native_manifest_hash"`
		NativeEnrolledAt     *string `json:"native_enrolled_at"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatalf("finalize receipt decode: %v", err)
	}
	if receipt.ID != sourceID || receipt.NativeEnrollmentID != enrollmentID || receipt.NativeEnrollmentStat != "enrolled" {
		t.Fatalf("finalize receipt identity wrong: source_ok=%t enrollment_ok=%t status=%q",
			receipt.ID == sourceID, receipt.NativeEnrollmentID == enrollmentID, receipt.NativeEnrollmentStat)
	}
	if receipt.NativeManifestHash == nil || *receipt.NativeManifestHash != hash {
		t.Fatal("enrolled receipt must pin the manifest hash")
	}
	if receipt.NativeEnrolledAt == nil {
		t.Fatal("enrolled receipt must carry native_enrolled_at")
	}

	// Exact replay with the STILL VALID capability: same terminal receipt.
	_, replay := mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, proof, http.StatusOK)
	var replayReceipt struct {
		ID                   string  `json:"id"`
		NativeEnrollmentID   string  `json:"native_enrollment_id"`
		NativeEnrollmentStat string  `json:"native_enrollment_status"`
		NativeManifestHash   *string `json:"native_manifest_hash"`
	}
	if err := json.Unmarshal(replay, &replayReceipt); err != nil {
		t.Fatalf("replay receipt decode: %v", err)
	}
	if replayReceipt.ID != receipt.ID || replayReceipt.NativeEnrollmentID != enrollmentID ||
		replayReceipt.NativeEnrollmentStat != "enrolled" || replayReceipt.NativeManifestHash == nil {
		t.Fatalf("terminal replay must return the same enrolled receipt: %+v", replayReceipt)
	}

	// Normal deletion is allowed and invalidates replay: the row is gone
	// (local files are a client concern, never remotely erased).
	mustSourceReadCall(t, ctx, http.MethodDelete, "/api/work-sources/"+sourceID, testToken, testWorkspaceID, "", http.StatusNoContent)
	mustSourceReadCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, proof, http.StatusNotFound)
}
