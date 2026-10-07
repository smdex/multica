package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Only a test-created CLI is executed. The script rejects anything except
// discovery, including thread/turn creation, and requires the full handshake.
func fakeCodexModelServer(t *testing.T, responses []string) (Command, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI requires a POSIX shell")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests")
	path := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1" = "wrapper" ] || exit 81
shift
case "$*" in
  --version) echo 'codex-cli 0.144.1'; exit 0 ;;
  'debug models --bundled')
    echo '{"models":[{"slug":"live","visibility":"list","supported_reasoning_levels":[{"effort":"WRONG"}],"service_tiers":[{"id":"priority","name":"Fast","description":"Priority routing"}]}]}'
    exit 0 ;;
  app-server) ;;
  *) exit 82 ;;
esac
IFS= read -r line || exit 83
case "$line" in *'"method":"initialize"'*'"clientInfo"'*) ;; *) exit 84 ;; esac
printf '%s\n' "$line" >> '` + logPath + `'
echo '{"method":"notice","params":{}}'
echo '{"id":999,"method":"item/commandExecution/requestApproval","params":{}}'
echo '{"id":999,"result":{"ignored":true}}'
echo '{"id":1,"result":{}}'
IFS= read -r line || exit 85
case "$line" in *'"method":"initialized"'*) ;; *) exit 86 ;; esac
printf '%s\n' "$line" >> '` + logPath + `'
n=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> '` + logPath + `'
  case "$line" in *'"method":"model/list"'*) ;; *) exit 87 ;; esac
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  n=$((n+1))
  case "$n" in
`
	for i, response := range responses {
		script += fmt.Sprintf("    %d) printf '{\"id\":%%s,%s}\\n' \"$id\" ;;\n", i+1, response)
	}
	script += "  esac\ndone\n"
	if len(responses) == 0 {
		script = strings.Replace(script, "IFS= read -r line || exit 83", "sleep 30 &\nprintf '%s %s\\n' \"$$\" \"$!\" > '"+logPath+".pids'\nIFS= read -r line || exit 83", 1)
	}
	writeTestExecutable(t, path, []byte(script))
	return NewCommand(path, []string{"wrapper"}), logPath
}

func TestCodexAppServerModelCatalog(t *testing.T) {
	cmd, logPath := fakeCodexModelServer(t, []string{
		`"result":{"data":[{"id":"live","displayName":"Live model","isDefault":false,"defaultReasoningEffort":"future","supportedReasoningEfforts":[{"reasoningEffort":"none","description":"Disabled"},{"reasoningEffort":"future","description":"Native effort"}]}],"nextCursor":"page-2"}`,
		`"result":{"data":[{"id":"plain","isDefault":true,"supportedReasoningEfforts":[]}],"nextCursor":null}`,
	})
	catalog, err := ListModels(context.Background(), "codex", cmd)
	if err != nil || catalog.Fallback || len(catalog.Models) != 2 {
		t.Fatalf("catalog = %+v, err = %v", catalog, err)
	}
	live, plain := catalog.Models[0], catalog.Models[1]
	want := &ModelThinking{SupportedLevels: []ThinkingLevel{
		{Value: "none", Label: "None", Description: "Disabled"},
		{Value: "future", Label: "Future", Description: "Native effort"},
	}, DefaultLevel: "future"}
	if live.ID != "live" || live.Label != "Live model" || live.Provider != "openai" || live.Default || !reflect.DeepEqual(live.Thinking, want) {
		t.Fatalf("live model = %+v, thinking = %+v", live, live.Thinking)
	}
	if plain.ID != "plain" || plain.Label != "plain" || !plain.Default || plain.Thinking != nil {
		t.Fatalf("plain model = %+v", plain)
	}
	if !live.SupportsExplicitStandardServiceTier || len(live.ServiceTiers) != 1 || live.ServiceTiers[0].Description != "Priority routing" {
		t.Fatalf("lost service metadata: %+v", live)
	}
	requests, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(requests), `"cursor":"page-2"`) {
		t.Fatalf("pagination requests = %s, err = %v", requests, err)
	}
}

func TestCodexAppServerInvalidCatalog(t *testing.T) {
	for name, responses := range map[string][]string{
		"rpc error":         {`"error":{"code":-32601,"message":"unsupported"}`},
		"empty":             {`"result":{"data":[]}`},
		"malformed":         {`"result":{"data":"wrong"}`},
		"missing id":        {`"result":{"data":[{"displayName":"invalid"}]}`},
		"missing data":      {`"result":{}`},
		"null result":       {`"result":null`},
		"malformed efforts": {`"result":{"data":[{"id":"live","supportedReasoningEfforts":"wrong"}]}`},
		"malformed cursor":  {`"result":{"data":[{"id":"live"}],"nextCursor":12}`},
		"timeout":           nil,
		"repeated cursor": {
			`"result":{"data":[{"id":"first"}],"nextCursor":"same"}`,
			`"result":{"data":[{"id":"second"}],"nextCursor":"same"}`,
		},
		"later page error": {
			`"result":{"data":[{"id":"first"}],"nextCursor":"next"}`,
			`"error":{"code":1,"message":"failed"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cmd, _ := fakeCodexModelServer(t, responses)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if models, err := discoverCodexAppServerModels(ctx, cmd); err == nil {
				t.Fatalf("invalid/partial catalog accepted: %+v", models)
			}
		})
	}
}

func TestCodexCatalogFallbackNotCached(t *testing.T) {
	cmd, logPath := fakeCodexModelServer(t, []string{`"error":{"code":1,"message":"offline"}`})
	for i := 0; i < 2; i++ {
		catalog, err := ListModels(context.Background(), "codex", cmd)
		if err != nil || !catalog.Fallback || len(catalog.Models) != 1 || catalog.Models[0].ID != "live" {
			t.Fatalf("bundled fallback = %+v, err = %v", catalog, err)
		}
	}
	requests, _ := os.ReadFile(logPath)
	if strings.Count(string(requests), `"method":"initialize"`) != 2 {
		t.Fatalf("fallback was cached: %s", requests)
	}
}

func TestRefreshModelsBypassesLocalCache(t *testing.T) {
	cmd, logPath := fakeCodexModelServer(t, []string{`"result":{"data":[{"id":"live"}]}`})
	for i := 0; i < 2; i++ {
		if _, err := ListModels(context.Background(), "codex", cmd); err != nil {
			t.Fatal(err)
		}
	}
	requests, _ := os.ReadFile(logPath)
	if strings.Count(string(requests), `"method":"initialize"`) != 1 {
		t.Fatalf("memo missed: %s", requests)
	}
	script, err := os.ReadFile(cmd.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, cmd.Path, []byte(strings.ReplaceAll(string(script), `"id":"live"`, `"id":"refreshed"`)))
	catalog, err := RefreshModels(context.Background(), "codex", cmd)
	if err != nil || catalog.Fallback || len(catalog.Models) != 1 || catalog.Models[0].ID != "refreshed" {
		t.Fatalf("refresh = %+v, %v", catalog, err)
	}
	cached, err := ListModels(context.Background(), "codex", cmd)
	if err != nil || !reflect.DeepEqual(cached, catalog) {
		t.Fatalf("refresh did not replace memo: %+v, %v", cached, err)
	}
	requests, _ = os.ReadFile(logPath)
	if strings.Count(string(requests), `"method":"initialize"`) != 2 {
		t.Fatalf("refresh used memo or failed to populate it: %s", requests)
	}
}

func TestCodexAppServerDiscoveryCancellation(t *testing.T) {
	cmd, logPath := fakeCodexModelServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := discoverCodexAppServerModels(ctx, cmd)
		done <- err
	}()
	waitForFile(t, logPath+".pids", 2*time.Second)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup blocked")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cleanup blocked: %s", elapsed)
	}
	pids, err := os.ReadFile(logPath + ".pids")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range strings.Fields(string(pids)) {
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		assertCursorTestProcessGone(t, pid)
	}
}

func TestCodexLiveDiscoveryDoesNotRequireVersionSupport(t *testing.T) {
	for _, version := range []string{"0.121.0", "unknown-version"} {
		t.Run(version, func(t *testing.T) {
			cmd, _ := fakeCodexModelServer(t, []string{`"result":{"data":[{"id":"live"}]}`})
			script, err := os.ReadFile(cmd.Path)
			if err != nil {
				t.Fatal(err)
			}
			writeTestExecutable(t, cmd.Path, []byte(strings.ReplaceAll(string(script), "0.144.1", version)))
			catalog, err := ListModels(context.Background(), "codex", cmd)
			if err != nil || catalog.Fallback || len(catalog.Models) != 1 || catalog.Models[0].ID != "live" {
				t.Fatalf("live catalog = %+v, %v", catalog, err)
			}
			if catalog.Models[0].SupportsExplicitStandardServiceTier {
				t.Fatal("unknown/old version claimed explicit standard support")
			}
		})
	}
}
