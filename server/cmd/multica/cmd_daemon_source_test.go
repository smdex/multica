package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
)

func nativeSourceTestCommand(t *testing.T, serverURL, daemonID string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	for key, value := range map[string]string{
		"server-url": serverURL, "daemon-id": daemonID, "profile": "native-test",
		"workspace": uuid.NewString(), "runtime": uuid.NewString(), "request-id": uuid.NewString(),
		"name": "native test", "source": uuid.NewString(),
	} {
		cmd.Flags().String(key, value, "")
	}
	_ = cmd.Flags().Set("server-url", serverURL)
	return cmd
}

func nativeSourceTestHumanEnvironment(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"MULTICA_AGENT_ID", "MULTICA_TASK_ID", "MULTICA_DAEMON_PORT", cli.TaskConfigRootEnv, "MULTICA_DAEMON_ID", "MULTICA_SERVER_URL"} {
		t.Setenv(key, "")
	}
	t.Setenv("MULTICA_TOKEN", "human-test-token")
	return home
}

func TestNativeSourceCLIRejectsManagedTasksBeforeHTTPOrFiles(t *testing.T) {
	for _, signal := range []string{cli.TaskConfigRootEnv, "MULTICA_AGENT_ID", "MULTICA_TASK_ID"} {
		t.Run(signal, func(t *testing.T) {
			home := nativeSourceTestHumanEnvironment(t)
			t.Setenv(signal, "task-context")
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("managed task must not send a human enrollment request")
			}))
			defer server.Close()
			cmd := nativeSourceTestCommand(t, server.URL, "test-daemon")
			for _, run := range []func(*cobra.Command, []string) error{runDaemonSourceProvision, runDaemonSourceApprove, runDaemonSourceStatus} {
				if err := run(cmd, nil); err == nil {
					t.Fatal("managed task enrollment accepted")
				}
			}
			if _, err := os.Stat(filepath.Join(home, ".multica")); !os.IsNotExist(err) {
				t.Fatalf("managed task must not create global identity or source files: %v", err)
			}
		})
	}
}

func TestNativeSourceCLIProvisionPreservesIntentAndGlobalDomainOnReplay(t *testing.T) {
	home := nativeSourceTestHumanEnvironment(t)
	source := daemon.NativeSourceEnrollment{
		ID: uuid.NewString(), WorkspaceID: uuid.NewString(), RuntimeID: uuid.NewString(),
		DaemonID: "native-cli-daemon", Name: "native test", Mode: "native", ConfigRevision: 1,
		NativeEnrollmentID: uuid.NewString(), NativeEnrollmentStatus: "pending",
		NativeOwnerMemberID: uuid.NewString(), NativeRuntimeCreatedAt: "2026-10-07T12:00:00Z",
	}
	source.SourceHandle = "managed:" + source.ID
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/runtimes/"+source.RuntimeID+"/source-enrollments" || r.Header.Get("X-Workspace-ID") != source.WorkspaceID || r.Header.Get("Authorization") != "Bearer human-test-token" {
			t.Error("provision request lost exact identity or human credential")
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(source)
	}))
	defer server.Close()
	cmd := nativeSourceTestCommand(t, server.URL, source.DaemonID)
	_ = cmd.Flags().Set("workspace", source.WorkspaceID)
	_ = cmd.Flags().Set("runtime", source.RuntimeID)
	var output bytes.Buffer
	cmd.SetOut(&output)
	for range 2 {
		if err := runDaemonSourceProvision(cmd, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected initial request and durable replay, got %d", calls)
	}
	local, err := daemon.LoadNativeSourceEnrollmentLocal("native-test", server.URL, source.WorkspaceID, source.ID)
	if err != nil || local.Identity.SourceID != source.ID || local.Identity.EnrollmentID != source.NativeEnrollmentID {
		t.Fatalf("replay changed the local ownership identity: %v", err)
	}
	marker := filepath.Join(home, ".multica", "native-sources", source.ID, ".native_owner")
	if info, err := os.Stat(marker); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("global private marker missing: %v", err)
	}
	var receipt daemon.NativeSourceEnrollment
	if err := json.NewDecoder(&output).Decode(&receipt); err != nil || receipt.ID != source.ID {
		t.Fatalf("CLI did not output its public source receipt: %v", err)
	}
}
