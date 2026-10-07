package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	nseRuntimeID  = "11111111-1111-4111-8111-111111111111"
	nseWorkspace  = "22222222-2222-4222-8222-222222222222"
	nseDaemonID   = "daemon-opaque-9k2!!qs"
	nseSourceID   = "66666666-6666-4666-8666-666666666666"
	nseRequestID  = "55555555-5555-4555-8555-555555555555"
	nseEnrollID   = "77777777-7777-4777-8777-777777777777"
	nseMemberID   = "88888888-8888-4888-8888-888888888888"
	nseHumanToken = "mul_human_native_test"
	nseMSEToken   = "mse_test_enrollment_token"
	nseManifest   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func newNSETestClient(t *testing.T) (*Client, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	c := NewClient(ts.URL)
	c.SetToken(nseHumanToken)
	return c, mux
}

func TestNativeSourceEnrollmentClientCompletedIntentReplay(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, http.StatusOK, nseEnrolledSource(true))
	})
	got, err := client.CreateNativeSourceIntent(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseRequestID, "docs")
	if err != nil || got.ID != nseSourceID || got.NativeEnrollmentStatus != "enrolled" {
		t.Fatalf("completed intent replay rejected: %v", err)
	}
}

func TestNativeSourceEnrollmentClientRejectsNarrowedRevisionAndForeignHandle(t *testing.T) {
	t.Run("revision overflow", func(t *testing.T) {
		client, mux := newNSETestClient(t)
		mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/token", func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(t, w, http.StatusOK, nseMintBody(time.Now(), func(body map[string]any) { body["config_revision"] = int64(3) + 1<<32 }))
		})
		if _, err := client.MintSourceEnrollmentToken(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, nseValidProof()); err == nil {
			t.Fatal("overflowing config revision accepted after narrowing")
		}
	})
	t.Run("foreign managed handle", func(t *testing.T) {
		client, mux := newNSETestClient(t)
		mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments", func(w http.ResponseWriter, r *http.Request) {
			body := nsePendingSource(false)
			body["source_handle"] = "managed:" + nseRuntimeID
			writeJSONBody(t, w, http.StatusCreated, body)
		})
		if _, err := client.CreateNativeSourceIntent(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseRequestID, "docs"); err == nil {
			t.Fatal("source handle belonging to another UUID accepted")
		}
	})
}

func nseValidProof() NativeSourceEnrollmentProof {
	return NativeSourceEnrollmentProof{EnrollmentID: nseEnrollID, ConfigRevision: 3, ManifestHash: nseManifest}
}

func nsePendingSource(enabled bool) map[string]any {
	return map[string]any{
		"id": nseSourceID, "workspace_id": nseWorkspace, "runtime_id": nseRuntimeID,
		"daemon_id": nseDaemonID, "name": "docs", "mode": "native", "enabled": enabled,
		"source_handle": "managed:" + nseSourceID, "config_revision": 3,
		"native_enrollment_id": nseEnrollID, "native_enrollment_status": "pending",
		"native_owner_member_id":    nseMemberID,
		"native_runtime_created_at": "2026-10-07T10:00:00Z",
	}
}

func nseEnrolledSource(enabled bool) map[string]any {
	s := nsePendingSource(enabled)
	s["native_enrollment_status"] = "enrolled"
	s["native_manifest_hash"] = nseManifest
	s["native_enrolled_at"] = "2026-10-07T11:00:00Z"
	return s
}

func nseMintBody(now time.Time, mutate func(map[string]any)) map[string]any {
	body := map[string]any{
		"token": nseMSEToken, "token_type": "Bearer", "scope": "source:enroll",
		"workspace_id": nseWorkspace, "runtime_id": nseRuntimeID, "daemon_id": nseDaemonID,
		"source_id": nseSourceID, "enrollment_id": nseEnrollID,
		"config_revision": 3, "manifest_hash": nseManifest,
		"expires_at": now.Add(90 * time.Second).UTC().Format(time.RFC3339), "expires_in": 90,
	}
	if mutate != nil {
		mutate(body)
	}
	return body
}

func writeJSONBody(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// decodeRequest captures the request wire format for contract assertions.
func decodeRequest(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

func TestNativeSourceEnrollmentClientCreateRequestContract(t *testing.T) {
	client, mux := newNSETestClient(t)
	var auth, workspace, contentType, method, path string
	var hasIdentity bool
	var body map[string]any
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments", func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		auth = r.Header.Get("Authorization")
		workspace = r.Header.Get("X-Workspace-ID")
		contentType = r.Header.Get("Content-Type")
		hasIdentity = r.Header.Get("X-Client-Platform") != ""
		body = decodeRequest(r)
		writeJSONBody(t, w, http.StatusCreated, nsePendingSource(false))
	})
	out, err := client.CreateNativeSourceIntent(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseRequestID, "docs")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if method != http.MethodPost || path != "/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments" {
		t.Fatalf("unexpected method/path: %s %s", method, path)
	}
	if auth != "Bearer "+nseHumanToken || workspace != nseWorkspace {
		t.Fatalf("request auth/workspace header violated: %q %q", auth, workspace)
	}
	if !strings.Contains(contentType, "application/json") || !hasIdentity {
		t.Fatalf("content type / identity headers missing")
	}
	if body["request_id"] != nseRequestID || body["name"] != "docs" {
		t.Fatalf("body must carry request_id and name: %v", body)
	}
	if out.NativeEnrollmentStatus != "pending" || out.Enabled || out.SourceHandle != "managed:"+nseSourceID {
		t.Fatalf("unexpected source: %+v", out)
	}
}

func TestNativeSourceEnrollmentClientCreateReplay200(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, http.StatusOK, nsePendingSource(false))
	})
	if _, err := client.CreateNativeSourceIntent(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseRequestID, "docs"); err != nil {
		t.Fatalf("replay 200 must be accepted: %v", err)
	}
}

func TestNativeSourceEnrollmentClientMintRequestAndValidation(t *testing.T) {
	client, mux := newNSETestClient(t)
	var auth, workspace string
	var body map[string]any
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/token", func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		workspace = r.Header.Get("X-Workspace-ID")
		body = decodeRequest(r)
		writeJSONBody(t, w, http.StatusOK, nseMintBody(time.Now(), nil))
	})
	proof := nseValidProof()
	cred, err := client.MintSourceEnrollmentToken(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, proof)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if auth != "Bearer "+nseHumanToken || workspace != nseWorkspace {
		t.Fatalf("mint must use human token + exact workspace header: %q %q", auth, workspace)
	}
	if body["enrollment_id"] != nseEnrollID || body["config_revision"] != float64(3) || body["manifest_hash"] != nseManifest {
		t.Fatalf("mint body must carry the proof: %v", body)
	}
	if cred.Token != nseMSEToken || cred.SourceID != nseSourceID || cred.EnrollmentID != nseEnrollID ||
		cred.ConfigRevision != 3 || cred.ManifestHash != nseManifest || cred.ExpiresIn != 90 {
		t.Fatalf("credential mismatch: %+v", cred)
	}
	if !cred.ExpiresAt.After(time.Now()) {
		t.Fatalf("credential must be future-dated: %v", cred.ExpiresAt)
	}
}

func TestNativeSourceEnrollmentClientMintRejectsBadTokens(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing prefix":  func(b map[string]any) { b["token"] = "notmse_token" },
		"blank token":     func(b map[string]any) { b["token"] = "" },
		"wrong scope":     func(b map[string]any) { b["scope"] = "source:read" },
		"wrong type":      func(b map[string]any) { b["token_type"] = "Basic" },
		"wrong runtime":   func(b map[string]any) { b["runtime_id"] = nseSourceID },
		"wrong workspace": func(b map[string]any) { b["workspace_id"] = nseSourceID },
		"wrong daemon":    func(b map[string]any) { b["daemon_id"] = "other" },
		"wrong source":    func(b map[string]any) { b["source_id"] = nseRequestID },
		"wrong proof id":  func(b map[string]any) { b["enrollment_id"] = nseRequestID },
		"wrong hash":      func(b map[string]any) { b["manifest_hash"] = strings.Repeat("0", 64) },
		"wrong revision":  func(b map[string]any) { b["config_revision"] = 4 },
		"expired": func(b map[string]any) {
			b["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			b["expires_in"] = 90
		},
		"too long": func(b map[string]any) {
			b["expires_at"] = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
			b["expires_in"] = 600
		},
		"expires_in zero": func(b map[string]any) { b["expires_in"] = 0 },
		"expires_in big":  func(b map[string]any) { b["expires_in"] = 121 },
		"bad expires_at":  func(b map[string]any) { b["expires_at"] = "yesterday" },
		"whitespace tok":  func(b map[string]any) { b["token"] = "mse_bad token" },
		"control tok":     func(b map[string]any) { b["token"] = "mse_bad\x01token" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client, mux := newNSETestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/token", func(w http.ResponseWriter, r *http.Request) {
				writeJSONBody(t, w, http.StatusOK, nseMintBody(time.Now(), mutate))
			})
			_, err := client.MintSourceEnrollmentToken(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, nseValidProof())
			if err == nil {
				t.Fatalf("%s: expected rejection", name)
			}
		})
	}
}

func TestNativeSourceEnrollmentClientMintTransportFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"malformed json":  {`{"token":`, http.StatusOK},
		"null body":       {`null`, http.StatusOK},
		"trailing json":   {`{"token":"mse_x"} {"token":"mse_y"}`, http.StatusOK},
		"array body":      {`[]`, http.StatusOK},
		"error status":    {`{"error":"secret diagnostic mse_real_token"}`, http.StatusUnauthorized},
		"server error":    {`boom`, http.StatusInternalServerError},
		"accepted status": {`{}`, http.StatusAccepted},
	} {
		t.Run(name, func(t *testing.T) {
			client, mux := newNSETestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/token", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := client.MintSourceEnrollmentToken(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, nseValidProof())
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if strings.Contains(err.Error(), "secret diagnostic") || strings.Contains(err.Error(), "mse_real_token") || strings.Contains(err.Error(), "boom") {
				t.Fatalf("error leaks response body or credential: %v", err)
			}
		})
	}
}

func TestNativeSourceEnrollmentClientFinalizeUsesExplicitMSEOnly(t *testing.T) {
	client, mux := newNSETestClient(t)
	var auth, workspace, method string
	var body map[string]any
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/finalize", func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		auth = r.Header.Get("Authorization")
		workspace = r.Header.Get("X-Workspace-ID")
		body = decodeRequest(r)
		writeJSONBody(t, w, http.StatusOK, nseEnrolledSource(true))
	})
	out, err := client.FinalizeNativeSourceEnrollment(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, nseMSEToken, nseValidProof())
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if method != http.MethodPost || auth != "Bearer "+nseMSEToken {
		t.Fatalf("finalize must POST with the explicit mse token, got %s %q", method, auth)
	}
	if workspace != nseWorkspace || body["enrollment_id"] != nseEnrollID || body["manifest_hash"] != nseManifest || body["config_revision"] != float64(3) {
		t.Fatalf("finalize request contract violated: %q %v", workspace, body)
	}
	if client.Token() != nseHumanToken {
		t.Fatalf("finalize must never mutate the human token, got %q", client.Token())
	}
	if out.NativeEnrollmentStatus != "enrolled" || !out.Enabled {
		t.Fatalf("finalized source must be enrolled (enabled allowed on retry): %+v", out)
	}
}

func TestNativeSourceEnrollmentClientFinalizeRejectsWrongCredential(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/finalize", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request may be made when the capability is missing")
	})
	for _, bad := range []string{"", nseHumanToken, "msr_other_capability", "mse_", "mse_bad token", "mse_bad\u0085token", "mse_bad\u2003token"} {
		if _, err := client.FinalizeNativeSourceEnrollment(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, bad, nseValidProof()); err == nil {
			t.Fatalf("token %q must be rejected before any request", bad)
		}
	}
}

func TestNativeSourceEnrollmentClientFinalizeRejectsWrongResponses(t *testing.T) {
	modified := func(f func(map[string]any)) map[string]any {
		s := nseEnrolledSource(false)
		f(s)
		return s
	}
	cases := map[string]map[string]any{
		"pending not enrolled":    nsePendingSource(false),
		"pending enabled":         nsePendingSource(true),
		"wrong source id":         modified(func(s map[string]any) { s["id"] = nseRequestID }),
		"wrong enrollment id":     modified(func(s map[string]any) { s["native_enrollment_id"] = nseRequestID }),
		"wrong manifest":          modified(func(s map[string]any) { s["native_manifest_hash"] = strings.Repeat("a", 64) }),
		"wrong revision":          modified(func(s map[string]any) { s["config_revision"] = 4 }),
		"missing member id":       modified(func(s map[string]any) { s["native_owner_member_id"] = "" }),
		"missing runtime created": modified(func(s map[string]any) { s["native_runtime_created_at"] = "" }),
		"non-managed handle":      modified(func(s map[string]any) { s["source_handle"] = "raw:/tmp" }),
		"wrong workspace":         modified(func(s map[string]any) { s["workspace_id"] = nseRequestID }),
		"wrong daemon":            modified(func(s map[string]any) { s["daemon_id"] = "other" }),
		"unknown status":          modified(func(s map[string]any) { s["native_enrollment_status"] = "zombie" }),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			client, mux := newNSETestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/finalize", func(w http.ResponseWriter, r *http.Request) {
				writeJSONBody(t, w, http.StatusOK, body)
			})
			if _, err := client.FinalizeNativeSourceEnrollment(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, nseMSEToken, nseValidProof()); err == nil {
				t.Fatalf("%s: expected rejection", name)
			}
		})
	}
}

func TestNativeSourceEnrollmentClientFinalizeTransportFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"malformed":    {`{`, http.StatusOK},
		"null":         {`null`, http.StatusOK},
		"trailing":     {`{} {}`, http.StatusOK},
		"error status": {`{"error":"secret"}`, http.StatusConflict},
		"created":      {`{}`, http.StatusCreated},
	} {
		t.Run(name, func(t *testing.T) {
			client, mux := newNSETestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/finalize", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := client.FinalizeNativeSourceEnrollment(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, nseMSEToken, nseValidProof())
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks response body: %v", err)
			}
		})
	}
}

func TestNativeSourceEnrollmentClientGetSelectsExactUUID(t *testing.T) {
	client, mux := newNSETestClient(t)
	var workspace, auth string
	mux.HandleFunc("/api/work-sources", func(w http.ResponseWriter, r *http.Request) {
		workspace = r.Header.Get("X-Workspace-ID")
		auth = r.Header.Get("Authorization")
		other := nsePendingSource(false)
		other["id"] = nseRequestID
		writeJSONBody(t, w, http.StatusOK, []map[string]any{other, nseEnrolledSource(true)})
	})
	out, err := client.GetNativeSourceEnrollment(context.Background(), nseWorkspace, nseSourceID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.ID != nseSourceID || out.NativeEnrollmentStatus != "enrolled" {
		t.Fatalf("wrong source selected: %+v", out)
	}
	if workspace != nseWorkspace || auth != "Bearer "+nseHumanToken {
		t.Fatalf("get request contract violated: %q %q", workspace, auth)
	}
}

func TestNativeSourceEnrollmentClientGetRejectsMissingAndMalformed(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"not found":      {`[]`, http.StatusOK},
		"malformed":      {`[`, http.StatusOK},
		"trailing":       {`[] []`, http.StatusOK},
		"error status":   {`{"error":"secret"}`, http.StatusForbidden},
		"null list":      {`null`, http.StatusOK},
		"created status": {`[]`, http.StatusCreated},
		"object list":    {`{}`, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			client, mux := newNSETestClient(t)
			mux.HandleFunc("/api/work-sources", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := client.GetNativeSourceEnrollment(context.Background(), nseWorkspace, nseSourceID)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks response body: %v", err)
			}
		})
	}
}

func TestNativeSourceEnrollmentClientInputValidation(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/api/work-sources", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid inputs must be rejected before any request")
	})
	ctx := context.Background()
	if _, err := client.CreateNativeSourceIntent(ctx, "not-a-uuid", nseWorkspace, nseDaemonID, nseRequestID, "x"); err == nil {
		t.Fatal("bad runtime id accepted")
	}
	if _, err := client.CreateNativeSourceIntent(ctx, nseRuntimeID, nseWorkspace, nseDaemonID, nseRequestID, "   "); err == nil {
		t.Fatal("blank name accepted")
	}
	if _, err := client.CreateNativeSourceIntent(ctx, nseRuntimeID, nseWorkspace, "", nseRequestID, "x"); err == nil {
		t.Fatal("blank daemon id accepted")
	}
	if _, err := client.CreateNativeSourceIntent(ctx, nseRuntimeID, nseWorkspace, nseDaemonID, "nope", "x"); err == nil {
		t.Fatal("non-uuid request id accepted")
	}
	badProof := NativeSourceEnrollmentProof{EnrollmentID: nseEnrollID, ConfigRevision: 0, ManifestHash: nseManifest}
	if _, err := client.MintSourceEnrollmentToken(ctx, nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, badProof); err == nil {
		t.Fatal("zero revision accepted")
	}
	badProof.ConfigRevision, badProof.ManifestHash = 3, "UPPERCASE"+strings.Repeat("a", 56)
	if _, err := client.MintSourceEnrollmentToken(ctx, nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, badProof); err == nil {
		t.Fatal("uppercase hash accepted")
	}
	badProof.ManifestHash = strings.Repeat("a", 63)
	if _, err := client.MintSourceEnrollmentToken(ctx, nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, badProof); err == nil {
		t.Fatal("short hash accepted")
	}
	badProof.ManifestHash, badProof.EnrollmentID = nseManifest, "nope"
	if _, err := client.MintSourceEnrollmentToken(ctx, nseRuntimeID, nseWorkspace, nseDaemonID, nseSourceID, badProof); err == nil {
		t.Fatal("non-uuid enrollment id accepted")
	}
	if _, err := client.GetNativeSourceEnrollment(ctx, nseWorkspace, "not-a-uuid"); err == nil {
		t.Fatal("non-uuid source selector accepted")
	}
}

func TestNativeSourceEnrollmentClientBodyCap(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", nativeEnrollmentMaxBodyBytes) + `"}`))
	})
	if _, err := client.CreateNativeSourceIntent(context.Background(), nseRuntimeID, nseWorkspace, nseDaemonID, nseRequestID, "docs"); err == nil {
		t.Fatal("oversized body accepted")
	}
}
