package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

// newFakeBD creates a test-created fake bd executable (never a user-installed
// CLI) that records its argv and BEADS_DIR, mirroring the beads package's own
// fixture pattern.
func newFakeBD(t *testing.T, script string) (exe, beadsDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd fixture is POSIX shell only")
	}
	dir := t.TempDir()
	exe = filepath.Join(dir, "bd")
	beadsDir = filepath.Join(dir, "src", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script = "#!/bin/sh\nFAKE_BD_DIR=\"" + strings.ReplaceAll(dir, `"`, `\"`) + "\"\n" + script
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, beadsDir
}

func noRunBD(t *testing.T) (exe, dir string) {
	t.Helper()
	return newFakeBD(t, `printf 'launched\n' > "$FAKE_BD_DIR/record"
exit 3`)
}

// assertNotLaunched fails if the fake's launch marker exists. The no-run fake
// writes the marker before exiting 3, so marker presence proves a launch.
func assertNotLaunched(t *testing.T, exe string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(filepath.Dir(exe), "record"))
	if err == nil {
		t.Fatal("executable was launched")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

func TestFakeBDWritesMarkerWhenRun(t *testing.T) {
	// Guards assertNotLaunched against vacuous passes: the no-run fake must
	// leave the marker behind when it actually executes.
	exe, _ := noRunBD(t)
	if out, err := exec.Command(exe).CombinedOutput(); err == nil {
		t.Fatalf("no-run fake should exit nonzero, output %s", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "record")); err != nil {
		t.Fatalf("fake must write launch marker when run: %v", err)
	}
}

func binding(exe, dir string) cli.WorkSourceReadBinding {
	return cli.WorkSourceReadBinding{
		WorkspaceID:  "01234567-89ab-cdef-0123-456789abcdef",
		SourceHandle: "primary",
		BeadsDir:     dir,
		Executable:   exe,
	}
}

var boundWS = "01234567-89ab-cdef-0123-456789abcdef"

func TestExecuteWorkSourceReadListArgvAndEnv(t *testing.T) {
	exe, dir := newFakeBD(t, `
printf '%s\n' "args:$*" "beads_dir:$BEADS_DIR" > "$FAKE_BD_DIR/record"
printf '[{"id":"bd-1","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0}]'
`)
	b := binding(exe, dir)
	out, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "list", Limit: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "[") {
		t.Fatalf("list must return a JSON array, got %s", out)
	}
	rec, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "record"))
	recs := string(rec)
	if !strings.Contains(recs, "list --json --flat -n 7") {
		t.Fatalf("unexpected argv: %q", recs)
	}
	if !strings.Contains(recs, "beads_dir:"+dir) {
		t.Fatalf("BEADS_DIR not forwarded: %q", recs)
	}
}

func TestExecuteWorkSourceReadReadObjectWithRevision(t *testing.T) {
	exe, dir := newFakeBD(t, `
printf '%s\n' "args:$*" "beads_dir:$BEADS_DIR" >> "$FAKE_BD_DIR/record"
if [ "$1" != "show" ]; then printf '[]'; exit; fi
printf '[{"id":"bd-1","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0,"revision":"rev-9"}]'
`)
	b := binding(exe, dir)
	out, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "read", NativeID: "bd-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "{") {
		t.Fatalf("read must return a JSON object, got %s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["revision"] != "rev-9" {
		t.Fatalf("missing revision: %v", obj)
	}
	rec, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "record"))
	if !strings.Contains(string(rec), "show --id=bd-1 --json") {
		t.Fatalf("unexpected argv: %q", rec)
	}
	want := "args:show --id=bd-1 --json\nbeads_dir:" + dir + "\nargs:--readonly --sandbox dep list --direction down --json -- bd-1 bd-1\nbeads_dir:" + dir + "\n"
	if string(rec) != want || obj["dependencies_complete"] != true || obj["dependency_count"] != float64(0) {
		t.Fatalf("two-call raw edge contract violated: %q / %v", rec, obj)
	}
}

func TestExecuteWorkSourceReadDependencyEvidenceSurvivesRemarshal(t *testing.T) {
	exe, dir := newFakeBD(t, `if [ "$1" = "show" ]; then
printf '[{"id":"bd-c","revision":"unchanged","dependency_count":99,"dependencies":[{"id":"spoof","dependency_type":"spoof"}],"dependencies_complete":true}]'
else
printf '[{"issue_id":"bd-c","depends_on_id":"bd-a","type":"blocks"},{"issue_id":"bd-c","depends_on_id":"bd-b","type":"blocks"}]'
fi`)
	out, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{binding(exe, dir)}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "read", NativeID: "bd-c",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{`"id":"bd-a","dependency_type":"blocks"`, `"id":"bd-b","dependency_type":"blocks"`, `"revision":"unchanged"`} {
		if !strings.Contains(string(out), retained) {
			t.Fatalf("daemon lost dependency evidence %s: %s", retained, out)
		}
	}
	if strings.Contains(string(out), `"title":"A"`) || strings.Contains(string(out), `"title":"B"`) {
		t.Fatalf("daemon must preserve only typed dependency identity and relation: %s", out)
	}
	if !strings.Contains(string(out), `"dependencies_complete":true`) || !strings.Contains(string(out), `"dependency_count":2`) || strings.Contains(string(out), "spoof") {
		t.Fatalf("raw evidence must replace show and claim qualified completeness: %s", out)
	}
}

func TestExecuteWorkSourceReadHostileNativeIDOneArgv(t *testing.T) {
	exe, dir := newFakeBD(t, `
if [ "$1" != "show" ]; then
  [ "$#" = 10 ] && [ "$9" = "bd-1; rm -rf /" ] && [ "${10}" = "$9" ] || exit 4
  printf '[]'; exit
fi
id=$(printf '%s' "$*" | sed 's/.*--id=//;s/ --json//')
printf '%s\n' "argc:$#" > "$FAKE_BD_DIR/record"
printf '[{"id":"'"$id"'","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0,"revision":"rev-9"}]'
`)
	b := binding(exe, dir)
	if _, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "read", NativeID: "bd-1; rm -rf /",
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "record"))
	if strings.TrimSpace(string(rec)) != "argc:3" { // show --id=... --json
		t.Fatalf("hostile ID must stay one argv element: %q", rec)
	}
}

func TestExecuteWorkSourceReadNoLaunchWithoutBinding(t *testing.T) {
	exe, dir := noRunBD(t)
	b := binding(exe, dir)
	for _, tc := range []struct{ name, ws, handle string }{
		{"wrong workspace", "ffffffff-ffff-ffff-ffff-ffffffffffff", "primary"},
		{"wrong handle", boundWS, "other"},
	} {
		if _, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
			WorkspaceID: tc.ws, SourceHandle: tc.handle, Command: "list",
		}); err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		assertNotLaunched(t, exe)
	}
}

func TestExecuteWorkSourceReadInvalidRequests(t *testing.T) {
	exe, dir := noRunBD(t)
	b := binding(exe, dir)
	bindings := []cli.WorkSourceReadBinding{b}
	cases := []struct {
		name string
		req  WorkSourceReadRequest
	}{
		{"write", WorkSourceReadRequest{WorkspaceID: boundWS, SourceHandle: "primary", Command: "update"}},
		{"empty command", WorkSourceReadRequest{WorkspaceID: boundWS, SourceHandle: "primary"}},
		{"read without native id", WorkSourceReadRequest{WorkspaceID: boundWS, SourceHandle: "primary", Command: "read"}},
		{"list with native id", WorkSourceReadRequest{WorkspaceID: boundWS, SourceHandle: "primary", Command: "list", NativeID: "bd-1"}},
		{"negative limit", WorkSourceReadRequest{WorkspaceID: boundWS, SourceHandle: "primary", Command: "list", Limit: -1}},
		{"over-limit", WorkSourceReadRequest{WorkspaceID: boundWS, SourceHandle: "primary", Command: "list", Limit: 201}},
	}
	for _, tc := range cases {
		if _, err := executeWorkSourceRead(context.Background(), bindings, tc.req); err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		assertNotLaunched(t, exe)
	}
}

func TestExecuteWorkSourceReadDecodeFailure(t *testing.T) {
	exe, dir := newFakeBD(t, "printf 'not json'")
	b := binding(exe, dir)
	if _, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "list",
	}); err == nil {
		t.Fatal("expected decode failure")
	}
}

func TestExecuteWorkSourceReadCancellation(t *testing.T) {
	exe, dir := newFakeBD(t, "sleep 30")
	b := binding(exe, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := executeWorkSourceRead(ctx, []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "list",
	}); err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancellation took too long")
	}
}

func TestExecuteWorkSourceReadReadRejectsLimitAndBlankNativeID(t *testing.T) {
	exe, dir := noRunBD(t)
	b := binding(exe, dir)
	bindings := []cli.WorkSourceReadBinding{b}
	for name, req := range map[string]WorkSourceReadRequest{
		"read with limit": {WorkspaceID: boundWS, SourceHandle: "primary", Command: "read", NativeID: "bd-1", Limit: 5},
		"blank native id": {WorkspaceID: boundWS, SourceHandle: "primary", Command: "read", NativeID: "   "},
	} {
		if _, err := executeWorkSourceRead(context.Background(), bindings, req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
		assertNotLaunched(t, exe)
	}
}

func TestExecuteWorkSourceReadFailsClosedOnInvalidBindings(t *testing.T) {
	exe, dir := noRunBD(t)
	uuid := boundWS
	cases := map[string][]cli.WorkSourceReadBinding{
		"relative beads_dir":  {{WorkspaceID: uuid, SourceHandle: "primary", BeadsDir: "rel/.beads", Executable: exe}},
		"relative executable": {{WorkspaceID: uuid, SourceHandle: "primary", BeadsDir: dir, Executable: "bd"}},
		"duplicate bindings": {
			{WorkspaceID: uuid, SourceHandle: "primary", BeadsDir: dir, Executable: exe},
			{WorkspaceID: uuid, SourceHandle: "primary", BeadsDir: dir, Executable: exe},
		},
	}
	for name, bindings := range cases {
		req := WorkSourceReadRequest{WorkspaceID: uuid, SourceHandle: "primary", Command: "list"}
		if _, err := executeWorkSourceRead(context.Background(), bindings, req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
		assertNotLaunched(t, exe)
	}
}

func TestExecuteWorkSourceReadListEnforcesRequestedLimitOnRows(t *testing.T) {
	exe, dir := newFakeBD(t, `
printf '[{"id":"bd-1","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0},{"id":"bd-2","title":"B","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0},{"id":"bd-3","title":"C","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0}]'
`)
	b := binding(exe, dir)
	if _, err := executeWorkSourceRead(context.Background(), []cli.WorkSourceReadBinding{b}, WorkSourceReadRequest{
		WorkspaceID: boundWS, SourceHandle: "primary", Command: "list", Limit: 2,
	}); err == nil {
		t.Fatal("expected over-limit row count rejection")
	}
}
