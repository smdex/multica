package beads

import (
	"context"
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
