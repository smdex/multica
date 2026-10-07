package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newFakeBD writes a test-created shell script (never a user-installed CLI)
// that emulates the qualified bd JSON contract. The script receives the
// fake's working directory in $FAKE_BD_DIR for recording argv/BEADS_DIR.
// approved returns a scratch BeadsDir so every fixture runs with the
// required approved source directory.
func newFakeBD(t *testing.T, script string) (exe, beadsDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd fixture is POSIX shell only")
	}
	dir := t.TempDir()
	exe = filepath.Join(dir, "bd")
	beadsDir = filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_BD_DIR", dir)
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nFAKE_BD_DIR=\""+dir+"\"\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, beadsDir
}

// fake is the every-test shorthand: newFakeBD plus a wired Client.
func fake(t *testing.T, script string) *Client {
	t.Helper()
	exe, dir := newFakeBD(t, script)
	return &Client{Executable: exe, BeadsDir: dir}
}

func TestListDecodesSummariesAndPassesArgv(t *testing.T) {
	exe, _ := newFakeBD(t, `
printf '%s\n' "args:$*" "beads_dir:$BEADS_DIR" > "$FAKE_BD_DIR/record"
printf '[{"id":"bd-1","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0,"unknown_future_field":true}]'
`)
	c := &Client{Executable: exe, BeadsDir: "/approved/.beads"}
	rows, err := c.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "bd-1" || rows[0].Title != "A" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	rec, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "record"))
	recs := string(rec)
	if !strings.Contains(recs, "-n 10") || !strings.Contains(recs, "beads_dir:/approved/.beads") {
		t.Fatalf("argv/env contract violated:\n%s", recs)
	}
}

func TestReadTaskDecodesRevision(t *testing.T) {
	c := fake(t, `
if [ "$1" != "show" ]; then printf '[]'; exit; fi
printf '[{"id":"bd-9","title":"Full","description":"body","status":"open","priority":1,"issue_type":"task","created_at":"t","updated_at":"t","revision":"-6978030324390938736"}]'
`)
	issue, err := c.ReadTask(context.Background(), "bd-9")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Revision == "" || issue.ID != "bd-9" || issue.Description == nil || *issue.Description != "body" {
		t.Fatalf("unexpected issue: %+v", issue)
	}
}

// TestReadTaskThroughQualifiedCLI is opt-in acceptance against an explicitly
// named disposable source. Ordinary tests never resolve or execute installed CLIs.
func TestReadTaskThroughQualifiedCLI(t *testing.T) {
	if os.Getenv("MULTICA_RUN_BEADS_QUALIFICATION") != "1" {
		t.Skip("explicit disposable Beads qualification only")
	}
	exe, dir := os.Getenv("MULTICA_BEADS_EXECUTABLE"), os.Getenv("MULTICA_BEADS_DIRECTORY")
	root, a, b, external := os.Getenv("MULTICA_BEADS_ROOT"), os.Getenv("MULTICA_BEADS_A"), os.Getenv("MULTICA_BEADS_B"), os.Getenv("MULTICA_BEADS_EXTERNAL")
	if !filepath.IsAbs(exe) || !filepath.IsAbs(dir) || root == "" || a == "" || b == "" || external == "" {
		t.Fatal("qualification requires absolute approved paths and all fixture selectors")
	}
	client := &Client{Executable: exe, BeadsDir: dir}
	issue, err := client.ReadTask(context.Background(), root)
	if err != nil {
		t.Fatalf("qualified root read failed: %v", err)
	}
	if !issue.DependenciesComplete || issue.DependencyCount != 3 || len(issue.Dependencies) != 3 {
		t.Fatal("qualified root must retain all three raw edges, including the external endpoint")
	}
	want := map[Dependency]bool{{ID: a, DependencyType: "blocks"}: true, {ID: b, DependencyType: "blocks"}: true, {ID: external, DependencyType: "blocks"}: true}
	for _, dependency := range issue.Dependencies {
		if !want[dependency] {
			t.Fatal("qualified read changed an exact endpoint or relation kind")
		}
		delete(want, dependency)
	}
	leaf, err := client.ReadTask(context.Background(), a)
	if err != nil || !leaf.DependenciesComplete || leaf.DependencyCount != 0 || len(leaf.Dependencies) != 0 {
		t.Fatal("qualified leaf must carry a complete empty edge observation")
	}
	payload, err := json.Marshal(leaf)
	if err != nil || !bytes.Contains(payload, []byte(`"dependencies":[]`)) || !bytes.Contains(payload, []byte(`"dependencies_complete":true`)) {
		t.Fatal("complete empty observation must survive canonical serialization")
	}
}

func TestReadTaskSharesWholeOperationDeadline(t *testing.T) {
	previous := defaultTimeout
	defaultTimeout = time.Second
	t.Cleanup(func() { defaultTimeout = previous })
	c := fake(t, `
sleep 0.6
if [ "$1" = "show" ]; then
  printf '[{"id":"bd-c","revision":"r1"}]'
else
  printf '[]'
fi
`)
	_, err := c.ReadTask(context.Background(), "bd-c")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("both subprocesses must share the operation deadline, got %v", err)
	}
}

func TestReadTaskDependencyEvidence(t *testing.T) {
	edge := `{"issue_id":"bd-c","depends_on_id":"bd-a","type":"blocks"}`
	for _, tc := range []struct {
		name, payload, stderr string
		exit, count           int
		bad                   bool
	}{
		{"join", `[` + edge + `,{"issue_id":"bd-c","depends_on_id":"bd-b","type":"blocks"}]`, "", 0, 2, false},
		{"empty", `[]`, "", 0, 0, false},
		{"external unknown exact", `[{"issue_id":"bd-c","depends_on_id":"external:project:capability","type":" Future Kind "}]`, "", 0, 1, false},
		{"same target different type", `[` + edge + `,{"issue_id":"bd-c","depends_on_id":"bd-a","type":"relates-to"}]`, "", 0, 2, false},
		{"warnings", `[` + edge + `]`, "private /source/path Bearer secret-token", 0, 0, true},
		{"missing anchor", `[]`, "warning: anchor not found", 0, 0, true},
		{"whitespace stderr", `[]`, " ", 0, 0, true},
		{"failed", `[]`, "private /source/path Bearer secret-token", 1, 0, true},
		{"failed stdout", `{"error":"private /source/path secret-token"}`, "", 1, 0, true},
		{"null", `null`, "", 0, 0, true},
		{"empty stdout", ``, "", 0, 0, true},
		{"trailing", `[] []`, "", 0, 0, true},
		{"malformed", `[private /source/path secret-token`, "", 0, 0, true},
		{"object", `{}`, "", 0, 0, true},
		{"foreign source", `[{"issue_id":"bd-other","depends_on_id":"bd-a","type":"blocks"}]`, "", 0, 0, true},
		{"blank source", `[{"issue_id":" ","depends_on_id":"bd-a","type":"blocks"}]`, "", 0, 0, true},
		{"blank target", `[{"issue_id":"bd-c","depends_on_id":" ","type":"blocks"}]`, "", 0, 0, true},
		{"blank type", `[{"issue_id":"bd-c","depends_on_id":"bd-a","type":" "}]`, "", 0, 0, true},
		{"missing type", `[{"issue_id":"bd-c","depends_on_id":"bd-a"}]`, "", 0, 0, true},
		{"null row", `[null]`, "", 0, 0, true},
		{"wrong target type", `[{"issue_id":"bd-c","depends_on_id":42,"type":"blocks"}]`, "", 0, 0, true},
		{"duplicate tuple", `[` + edge + `,` + edge + `]`, "", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake(t, fmt.Sprintf(`
if [ "$1" = "show" ]; then
  printf 'show warning is not a raw warning' >&2
  printf '[{"id":"bd-c","revision":"unchanged","dependency_count":99,"dependencies":[{"id":"spoof","dependency_type":"spoof"}],"dependencies_complete":true}]'
else
  printf '%%s' '%s'
  printf '%%s' '%s' >&2
  exit %d
fi`, tc.payload, tc.stderr, tc.exit))
			issue, err := c.ReadTask(context.Background(), "bd-c")
			if tc.bad {
				if err == nil || issue.DependenciesComplete || issue.ID != "" {
					t.Fatalf("raw failure must return no issue: %+v / %v", issue, err)
				}
				for _, private := range []string{"secret-token", "/source/path", c.Executable, c.BeadsDir} {
					if strings.Contains(err.Error(), private) {
						t.Fatalf("raw error leaked source diagnostic: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !issue.DependenciesComplete || issue.DependencyCount != tc.count || len(issue.Dependencies) != tc.count {
				t.Fatalf("raw evidence did not replace show: %+v", issue)
			}
			if tc.name == "external unknown exact" && (issue.Dependencies[0].ID != "external:project:capability" || issue.Dependencies[0].DependencyType != " Future Kind ") {
				t.Fatalf("opaque edge changed: %+v", issue.Dependencies)
			}
			raw, err := json.Marshal(issue)
			if err != nil || (tc.count == 0 && !strings.Contains(string(raw), `"dependencies":[]`)) {
				t.Fatalf("complete empty edges must marshal explicitly: %s / %v", raw, err)
			}
		})
	}
}

func TestReadTaskDependencyBound(t *testing.T) {
	for _, count := range []int{MaxDependencies, MaxDependencies + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			edges := make([]string, count)
			for i := range edges {
				edges[i] = fmt.Sprintf(`{"issue_id":"bd-c","depends_on_id":"bd-%d","type":"blocks"}`, i)
			}
			c := fake(t, `if [ "$1" = "show" ]; then printf '[{"id":"bd-c","revision":"r"}]'; else printf '%s' '[`+strings.Join(edges, ",")+`]'; fi`)
			issue, err := c.ReadTask(context.Background(), "bd-c")
			if (err == nil) != (count == MaxDependencies) {
				t.Fatalf("count=%d issue=%+v err=%v", count, issue, err)
			}
		})
	}
}

func TestReadTaskRawArgvAndEnv(t *testing.T) {
	native := " --opaque; $(not-a-shell) "
	encoded, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	c := fake(t, `printf '%s\n' "$BEADS_DIR" "$@" >> "$FAKE_BD_DIR/record"
if [ "$1" = "show" ]; then printf '%s' '[{"id":`+string(encoded)+`,"revision":"r"}]'; else printf '[]'; fi`)
	if _, err := c.ReadTask(context.Background(), native); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(filepath.Join(filepath.Dir(c.Executable), "record"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{c.BeadsDir, "show", "--id=" + native, "--json", c.BeadsDir, "--readonly", "--sandbox", "dep", "list", "--direction", "down", "--json", "--", native, native, ""}, "\n")
	if string(record) != want {
		t.Fatalf("exact argv/env contract: got %q want %q", record, want)
	}
}

func TestReadTaskRawCancellationAndOutputCap(t *testing.T) {
	t.Run("caller context", func(t *testing.T) {
		c := fake(t, `if [ "$1" = "show" ]; then printf '[{"id":"bd-c","revision":"r"}]'; else exec sleep 5; fi`)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		issue, err := c.ReadTask(ctx, "bd-c")
		if !errors.Is(err, context.DeadlineExceeded) || issue.DependenciesComplete {
			t.Fatalf("raw read must use caller context: %+v / %v", issue, err)
		}
	})
	t.Run("output cap", func(t *testing.T) {
		c := fake(t, `if [ "$1" = "show" ]; then printf '[{"id":"bd-c","revision":"r"}]'; else head -c 5000000 /dev/zero; fi`)
		issue, err := c.ReadTask(context.Background(), "bd-c")
		if err == nil || issue.DependenciesComplete {
			t.Fatalf("oversized raw read must fail closed: %+v / %v", issue, err)
		}
	})
}

func TestReadTaskNotFoundFromStructuredError(t *testing.T) {
	c := fake(t, `
printf '{"error":"no issues found matching the provided IDs","hint":"try bd history","schema_version":1}'
exit 1
`)
	_, err := c.ReadTask(context.Background(), "bd-404")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReadTaskEmptyRowsIsNotFound(t *testing.T) {
	c := fake(t, `printf '[]'`)
	if _, err := c.ReadTask(context.Background(), "bd-404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReadTaskRejectsAmbiguousRows(t *testing.T) {
	c := fake(t, `printf '[{"id":"bd-1"},{"id":"bd-2"}]'`)
	if _, err := c.ReadTask(context.Background(), "bd-1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want contract-violation error, got %v", err)
	}
}

func TestMalformedPayloadFailsClosed(t *testing.T) {
	c := fake(t, `printf 'not json at all'`)
	if _, err := c.List(context.Background(), 0); err == nil {
		t.Fatal("want decode error")
	}
}

func TestNullPayloadFailsClosed(t *testing.T) {
	c := fake(t, `printf 'null'`)
	if _, err := c.List(context.Background(), 10); err == nil {
		t.Error("null list payload must fail, not become an empty source")
	}
	if _, err := c.ReadTask(context.Background(), "bd-9"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("null detail payload must be a contract error, got %v", err)
	}
}

func TestListMissingIDFailsClosed(t *testing.T) {
	c := fake(t, `printf '[{"title":"no id"}]'`)
	if _, err := c.List(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "missing id") {
		t.Fatalf("want missing-id error, got %v", err)
	}
}

func TestListDuplicateIDFailsClosed(t *testing.T) {
	c := fake(t, `printf '[{"id":"bd-1"},{"id":"bd-1"}]'`)
	if _, err := c.List(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("want duplicate-id error, got %v", err)
	}
}

func TestReadTaskMismatchedIDFailsClosed(t *testing.T) {
	c := fake(t, `printf '[{"id":"bd-other","revision":"1"}]'`)
	if _, err := c.ReadTask(context.Background(), "bd-9"); err == nil || !strings.Contains(err.Error(), "bd-other") {
		t.Fatalf("want id-mismatch error, got %v", err)
	}
}

func TestReadTaskMissingRevisionFailsClosed(t *testing.T) {
	c := fake(t, `printf '[{"id":"bd-9"}]'`)
	if _, err := c.ReadTask(context.Background(), "bd-9"); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("want missing-revision error, got %v", err)
	}
}

func TestNonJSONFailureCarriesBoundedDetail(t *testing.T) {
	c := fake(t, `echo "boom: source on fire"; exit 1`)
	_, err := c.List(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want bounded detail error, got %v", err)
	}
}

func TestContextCancelPropagates(t *testing.T) {
	c := fake(t, `sleep 5`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.List(ctx, 0); err == nil {
		t.Fatal("want timeout error")
	}
}

func TestNoExecutableConfigured(t *testing.T) {
	c := &Client{BeadsDir: "/approved/.beads"}
	if _, err := c.List(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("want config error, got %v", err)
	}
}

func TestMissingSourceDirRejected(t *testing.T) {
	// Empty BeadsDir means bd cwd auto-discovery: never allowed at the
	// shared boundary, regardless of a configured executable.
	exe, _ := newFakeBD(t, `printf '[]'`)
	c := &Client{Executable: exe}
	if _, err := c.List(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "source directory") {
		t.Fatalf("want source-dir rejection, got %v", err)
	}
}

func TestErrorDetailIsRedacted(t *testing.T) {
	// Source diagnostics are untrusted: a bearer token echoed in stderr or
	// a structured error/hint must not survive into returned errors.
	c := fake(t, `echo "token: ghp_0123456789abcdefghijklmnopqrstuvwxyz and Bearer sk-live-abcdefghijklmnop on stderr"; exit 1`)
	_, err := c.List(context.Background(), 0)
	if err == nil {
		t.Fatal("want error")
	}
	if s := err.Error(); strings.Contains(s, "ghp_0123456789") || strings.Contains(s, "sk-live-abcdefghijklmnop") {
		t.Fatalf("unredacted secret in error: %s", s)
	}

	c2 := fake(t, `printf '{"error":"fetch failed for ghp_0123456789abcdefghijklmnopqrstuvwxyz","hint":"check token ghp_0123456789abcdefghijklmnopqrstuvwxyz","schema_version":1}'; exit 1`)
	_, err = c2.List(context.Background(), 0)
	if err == nil {
		t.Fatal("want error")
	}
	if s := err.Error(); strings.Contains(s, "ghp_0123456789") {
		t.Fatalf("unredacted secret in structured error/hint: %s", s)
	}
}

func TestDefaultTimeoutBareContext(t *testing.T) {
	// A caller with no deadline still gets the bounded default timeout.
	prev := defaultTimeout
	defaultTimeout = 300 * time.Millisecond
	t.Cleanup(func() { defaultTimeout = prev })
	c := fake(t, `sleep 600`)
	start := time.Now()
	if _, err := c.List(context.Background(), 0); err == nil {
		t.Fatal("want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("default timeout too lax: %v", elapsed)
	}
}

func TestEmptyNativeIDRejected(t *testing.T) {
	c := fake(t, `exit 0`)
	if _, err := c.ReadTask(context.Background(), "  "); err == nil {
		t.Fatal("want empty-ID error")
	}
}

func TestCallerDeadlinePreservedOverDefault(t *testing.T) {
	prev := defaultTimeout
	defaultTimeout = 10 * time.Second
	t.Cleanup(func() { defaultTimeout = prev })
	c := fake(t, `sleep 5`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.List(ctx, 0); err == nil {
		t.Fatal("want timeout error from caller deadline")
	}
	// Allow the 2s WaitDelay pipe-abandon on top of the 200ms deadline;
	// the 10s default must never be the effective bound.
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("caller deadline not preserved: %v", elapsed)
	}
}

func TestFarFutureDeadlineStillCapped(t *testing.T) {
	// A distant caller deadline must not disable the default cap:
	// WithTimeout installs the earlier default deadline alongside it.
	prev := defaultTimeout
	defaultTimeout = 300 * time.Millisecond
	t.Cleanup(func() { defaultTimeout = prev })
	c := fake(t, `sleep 10`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	start := time.Now()
	if _, err := c.List(ctx, 0); err == nil {
		t.Fatal("want timeout error despite far-future caller deadline")
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("default cap not applied over far-future deadline: %v", elapsed)
	}
}

func TestStructuredErrorCapped(t *testing.T) {
	// Structured error/hint fields are redacted AND bounded: a hostile bd
	// cannot balloon the returned error string.
	long := strings.Repeat("x", 5000)
	c := fake(t, fmt.Sprintf(`printf '{"error":"%s","hint":"%s","schema_version":1}'; exit 1`, long, long))
	_, err := c.List(context.Background(), 0)
	if err == nil {
		t.Fatal("want structured error")
	}
	if n := len(err.Error()); n > 1200 {
		t.Fatalf("structured error not capped: %d bytes", n)
	}
}

func TestOversizedOutputFailsClosed(t *testing.T) {
	c := fake(t, `yes x`)
	if _, err := c.List(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want bounded-output error, got %v", err)
	}
}

func TestExecutionIsArgvOnly(t *testing.T) {
	// An executable whose filename contains shell metacharacters must be
	// exec'd as a path, proving no shell interpretation happens.
	dir := t.TempDir()
	exe := filepath.Join(dir, "bd; echo pwned")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{Executable: exe, BeadsDir: filepath.Join(dir, ".beads")}
	if err := os.MkdirAll(c.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(context.Background(), 0); err != nil {
		t.Fatalf("argv exec should work without a shell: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); !os.IsNotExist(err) {
		t.Fatal("shell was invoked")
	}
}

// TestMain intentionally does not stub PATH or exec.LookPath: every
// fixture passes an absolute test-created executable, so a user-installed
// bd can never be resolved by these tests.

func TestListRejectsWhitespaceOnlyID(t *testing.T) {
	c := fake(t, `printf '[{"id":"  ","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0}]'`)
	if _, err := c.List(context.Background(), 10); err == nil {
		t.Fatal("whitespace-only id must be rejected, matching the receipt service's TrimSpace check")
	}
	// Preserve opaque non-blank bytes: surrounding spaces inside a nonblank ID
	// are data, not blankness.
	c2 := fake(t, `printf '[{"id":" bd-1 ","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0}]'`)
	rows, err := c2.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ID != " bd-1 " {
		t.Fatalf("opaque id must be preserved verbatim, got %q", rows[0].ID)
	}
}

func TestReadTaskRejectsWhitespaceOnlyRevision(t *testing.T) {
	c := fake(t, `printf '[{"id":"bd-9","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0,"revision":"  "}]'`)
	if _, err := c.ReadTask(context.Background(), "bd-9"); err == nil {
		t.Fatal("whitespace-only revision must be rejected, matching the receipt service's TrimSpace check")
	}
	// Nonblank revision preserved verbatim (opaque bytes).
	c2 := fake(t, `if [ "$1" != "show" ]; then printf '[]'; exit; fi
printf '[{"id":"bd-9","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0,"revision":" r1 "}]'`)
	issue, err := c2.ReadTask(context.Background(), "bd-9")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Revision != " r1 " {
		t.Fatalf("opaque revision must be preserved verbatim, got %q", issue.Revision)
	}
}
