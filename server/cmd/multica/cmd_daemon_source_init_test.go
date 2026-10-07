package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestNativeSourceCLIInitializerRejectsInvalidExecutableBeforeIntent(t *testing.T) {
	home := nativeSourceTestHumanEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid executable must not create a server intent")
	}))
	defer server.Close()
	for _, executable := range []string{"bd", home + "/../bd"} {
		t.Run(executable, func(t *testing.T) {
			cmd := nativeSourceTestCommand(t, server.URL, "explicit-daemon")
			cmd.Flags().String("beads-executable", executable, "")
			if err := runDaemonSourceProvision(cmd, nil); err == nil {
				t.Fatal("invalid initializer executable accepted")
			}
		})
	}
}

func TestNativeSourceCLIInitializerRejectsManagedTaskBeforeIntentOrFiles(t *testing.T) {
	home := nativeSourceTestHumanEnvironment(t)
	t.Setenv(cli.TaskConfigRootEnv, "managed-task")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("managed task must not authorize initialization")
	}))
	defer server.Close()
	cmd := nativeSourceTestCommand(t, server.URL, "explicit-daemon")
	cmd.Flags().String("beads-executable", filepath.Join(home, "missing-bd"), "")
	if err := runDaemonSourceProvision(cmd, nil); err == nil {
		t.Fatal("managed task initializer accepted")
	}
	if _, err := os.Stat(filepath.Join(home, ".multica")); !os.IsNotExist(err) {
		t.Fatalf("managed task touched human configuration: %v", err)
	}
}
