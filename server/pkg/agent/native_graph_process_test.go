//go:build linux

package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// nativeGraphAcceptanceGate is the explicit opt-in for the real namespace
// acceptance tests. With the gate set but namespaces unavailable, tests FAIL.
const nativeGraphAcceptanceGate = "MULTICA_NATIVE_GRAPH_PROCESS_TEST"

func requireNativeGraphAcceptance(t *testing.T) {
	t.Helper()
	if os.Getenv(nativeGraphAcceptanceGate) != "1" {
		t.Skipf("%s=1 required for native graph process acceptance", nativeGraphAcceptanceGate)
	}
	if err := probeNativeGraphNamespace(); err != nil {
		t.Fatalf("%s=1 but user namespaces are unavailable here (%v); fail closed, not skip", nativeGraphAcceptanceGate, err)
	}
}

// probeNativeGraphNamespace proves the actual clone path works with the same
// identity maps the production seam uses; a denied kernel surfaces as the
// real syscall error.
func probeNativeGraphNamespace() error {
	truePath, err := exec.LookPath("true")
	if err != nil {
		return err
	}
	s := exec.Command(truePath)
	if err := configureNativeGraphProcess(s); err != nil {
		return err
	}
	return s.Run()
}

// writeNativeGraphFakeClaude writes a fake Claude CLI: as PID-namespace init
// it spawns a setsid grandchild that escapes the process group, then either
// completes the stream-json protocol or hangs. The grandchild's output stays
// off the protocol pipe (holding it open would deadlock the scanner until
// WaitDelay); the test discovers the grandchild via its /proc cmdline.
func writeNativeGraphFakeClaude(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude")
	writeTestExecutable(t, path, []byte(strings.Join([]string{
		"#!/bin/sh",
		`if [ "$1" = "grandchild" ]; then`,
		"  sleep 300",
		"  exit 0",
		"fi",
		// UID preserved by the identity mapping (host uid baked in).
		`[ "$(id -u)" = "` + strconv.Itoa(os.Getuid()) + `" ] || { echo "uid map not preserved" >&2; exit 9; }`,
		`[ "$1" = "-p" ] || { echo "unexpected argv: $*" >&2; exit 8; }`,
		`setsid "$0" grandchild > /dev/null 2>&1 &`,
		`echo '{"type":"system","subtype":"init","session_id":"ns-test-session"}'`,
		// Keep the namespace init alive so the test can observe the escaped
		// grandchild before it completes (normal) or while hanging (cancel).
		`case "$NATIVE_GRAPH_FAKE_MODE" in hang) sleep 300 ;; *) sleep 2 ;; esac`,
		`echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello from namespace"}]}}'`,
		`echo '{"type":"result","subtype":"success","session_id":"ns-test-session","result":"done","is_error":false,"duration_ms":1,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":2}}'`,
	}, "\n")))
	return path
}

// waitForGrandchildPidfd scans /proc for the escaped grandchild by its argv
// ("<path> grandchild") and opens a pidfd on the pid visible at this procfs
// level. A pid read from the grandchild's own NSpid line can name an
// ancestor-namespace id on nested hosts and be unusable here.
func waitForGrandchildPidfd(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			t.Fatalf("read /proc: %v", err)
		}
		for _, e := range entries {
			pid, atoiErr := strconv.Atoi(e.Name())
			if atoiErr != nil {
				continue
			}
			cmdline, readErr := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if readErr != nil {
				continue
			}
			args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
			// A shell script re-execs as "/bin/sh <script> grandchild"; the fake's
			// path appears as an argument, not argv0.
			if len(args) < 2 || !slices.Contains(args, marker) || args[len(args)-1] != "grandchild" {
				continue
			}
			fd, openErr := unix.PidfdOpen(pid, 0)
			if openErr != nil {
				t.Fatalf("pidfd open for /proc pid %d (%q): %v", pid, cmdline, openErr)
			}
			return fd
		}
		if time.Now().After(deadline) {
			t.Fatal("escaped grandchild never became visible under /proc")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pidfdAlive/pidfdDead poll the retained pidfd. The alive-before/dead-after
// pair rules out a vacuous death check on an already-dead process.
func pidfdAlive(fd int) bool {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(poll, 0)
	return err == nil && n == 0
}

func pidfdDead(fd int, timeout time.Duration) bool {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(poll, int(timeout.Milliseconds()))
	return err == nil && n == 1 && poll[0].Revents&unix.POLLIN != 0
}

func TestNativeGraphProcessInvalidPidfdIsNotStopEvidence(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	if pidfdDead(fd, 0) {
		t.Fatal("an invalid pidfd cannot prove that a process stopped")
	}
}

func collectNativeGraphResult(t *testing.T, s *Session) Result {
	t.Helper()
	for range s.Messages {
	}
	select {
	case res, ok := <-s.Result:
		if !ok {
			t.Fatal("Result channel closed without a value")
		}
		return res
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for Result")
		return Result{}
	}
}

func newNativeGraphBackend(fakeClaude string, extraEnv map[string]string) *claudeBackend {
	env := map[string]string{"NATIVE_GRAPH_FAKE_MODE": "normal"}
	for k, v := range extraEnv {
		env[k] = v
	}
	return &claudeBackend{cfg: Config{
		ExecutablePath: fakeClaude,
		Logger:         slog.New(slog.DiscardHandler),
		Env:            env,
	}}
}

// TestNativeGraphProcessNormalCompletionRunsInNamespace: fake Claude runs as
// the namespace init with UID preserved, an escaped setsid grandchild is
// observed ALIVE via its pidfd before completion, then DEAD after the actual
// Wait reaped the init; Result on the same Session carries positive
// NativeProcessStopped.
func TestNativeGraphProcessNormalCompletionRunsInNamespace(t *testing.T) {
	requireNativeGraphAcceptance(t)
	dir := t.TempDir()
	fakeClaude := writeNativeGraphFakeClaude(t, dir)
	b := newNativeGraphBackend(fakeClaude, nil)

	var launchCount atomic.Int32
	s, err := b.Execute(context.Background(), "hello", ExecOptions{
		NativeGraphProcess: true,
		BeforeLaunch:       func() error { launchCount.Add(1); return nil },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	fd := waitForGrandchildPidfd(t, fakeClaude)
	defer unix.Close(fd)
	if !pidfdAlive(fd) {
		t.Fatal("grandchild pidfd not alive before completion — vacuous death check")
	}

	res := collectNativeGraphResult(t, s)
	if launchCount.Load() != 1 {
		t.Fatalf("BeforeLaunch ran %d times, want exactly 1", launchCount.Load())
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed (%+v)", res.Status, res)
	}
	if !res.NativeProcessStopped {
		t.Fatal("positive NativeProcessStopped missing on normal completion")
	}
	if !pidfdDead(fd, 10*time.Second) {
		t.Fatal("escaped setsid grandchild still alive after namespace init was reaped")
	}
}

// TestNativeGraphProcessCancellationStopsEscapedGrandchild: cancellation kills
// the hanging namespace init through the existing group path; the setsid
// grandchild — unreachable by group signals — must still die because the
// reaped init takes the namespace with it. NativeProcessStopped stays
// positive: the actual cmd.Wait returned even though no semantic result ran.
func TestNativeGraphProcessCancellationStopsEscapedGrandchild(t *testing.T) {
	requireNativeGraphAcceptance(t)
	dir := t.TempDir()
	fakeClaude := writeNativeGraphFakeClaude(t, dir)
	b := newNativeGraphBackend(fakeClaude, map[string]string{"NATIVE_GRAPH_FAKE_MODE": "hang"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := b.Execute(ctx, "hello", ExecOptions{
		NativeGraphProcess: true,
		BeforeLaunch:       func() error { return nil },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	fd := waitForGrandchildPidfd(t, fakeClaude)
	defer unix.Close(fd)
	if !pidfdAlive(fd) {
		t.Fatal("grandchild pidfd not alive before cancellation — vacuous death check")
	}

	cancel()

	res := collectNativeGraphResult(t, s)
	// Existing terminal mapping labels a cancelled run with no result
	// "aborted"; the stop evidence below is what this test owns.
	if res.Status != "cancelled" && res.Status != "aborted" {
		t.Fatalf("status = %q, want cancelled/aborted (%+v)", res.Status, res)
	}
	if !res.NativeProcessStopped {
		t.Fatal("positive NativeProcessStopped missing after cancelled namespace-init Wait")
	}
	if !pidfdDead(fd, 10*time.Second) {
		t.Fatal("escaped setsid grandchild survived cancellation of the namespace init")
	}
}

// writeMarkerFakeClaude writes a fake that touches marker whenever the
// provider-mode branch actually runs (grandchild excluded).
func writeMarkerFakeClaude(t *testing.T, dir string) (string, string) {
	t.Helper()
	fakeClaude := filepath.Join(dir, "fake-claude")
	marker := filepath.Join(dir, "launched")
	writeTestExecutable(t, fakeClaude, []byte(strings.Join([]string{
		"#!/bin/sh",
		`[ "$1" = "grandchild" ] || : > "` + marker + `"`,
		"sleep 300",
	}, "\n")))
	return fakeClaude, marker
}

// TestNativeGraphProcessBeforeLaunchFailureProvesNoLaunch: failing
// reservation callback aborts never-launched and the provider executable
// provably never ran.
func TestNativeGraphProcessBeforeLaunchFailureProvesNoLaunch(t *testing.T) {
	requireNativeGraphAcceptance(t)
	dir := t.TempDir()
	fakeClaude, marker := writeMarkerFakeClaude(t, dir)
	b := newNativeGraphBackend(fakeClaude, nil)

	_, err := b.Execute(context.Background(), "hello", ExecOptions{
		NativeGraphProcess: true,
		BeforeLaunch:       func() error { return errors.New("reservation fsync failed") },
	})
	if !errors.Is(err, ErrNativeGraphNeverLaunched) {
		t.Fatalf("error not classified never-launched: %v", err)
	}
	if !strings.Contains(err.Error(), "reservation fsync failed") {
		t.Fatalf("underlying BeforeLaunch error lost: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("provider executable ran despite BeforeLaunch failure (marker stat err: %v)", statErr)
	}
}

// TestNativeGraphProcessMarkerPositiveControl proves the marker mechanism
// itself: when the launch does proceed, the fake's provider branch writes the
// marker. Without this, an absent marker in the failure tests proves nothing.
// The fake hangs after writing; a cancellable ctx reaps it promptly.
func TestNativeGraphProcessMarkerPositiveControl(t *testing.T) {
	requireNativeGraphAcceptance(t)
	dir := t.TempDir()
	fakeClaude, marker := writeMarkerFakeClaude(t, dir)
	b := newNativeGraphBackend(fakeClaude, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := b.Execute(ctx, "hello", ExecOptions{
		NativeGraphProcess: true,
		BeforeLaunch:       func() error { return nil },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(marker); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			select {
			case res := <-s.Result:
				t.Fatalf("marker never written before run ended: %+v", res)
			default:
			}
			t.Fatal("launched fake never wrote the marker — marker mechanism broken")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	collectNativeGraphResult(t, s)
}

// TestNativeGraphProcessMissingCallbackFailsClosed: missing reservation both
// errors never-launched AND proves zero launch via the marker.
func TestNativeGraphProcessMissingCallbackFailsClosed(t *testing.T) {
	requireNativeGraphAcceptance(t)
	dir := t.TempDir()
	fakeClaude, marker := writeMarkerFakeClaude(t, dir)
	b := newNativeGraphBackend(fakeClaude, nil)

	_, err := b.Execute(context.Background(), "hello", ExecOptions{NativeGraphProcess: true})
	if !errors.Is(err, ErrNativeGraphNeverLaunched) {
		t.Fatalf("missing BeforeLaunch must fail closed as never-launched, got: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("missing BeforeLaunch still launched the provider (marker stat err: %v)", statErr)
	}
}

// TestNativeGraphProcessStartFailuresClassifiedNeverLaunched covers the two
// synchronous launch-boundary failure classes in one place:
//  1. LookPath-rejected executables — missing path, non-executable 0644
//     file, and a directory (Go's LookPath rejects all three) — BeforeLaunch
//     must NOT run (0 calls proves the failure preceded it).
//  2. A Start-time exec failure that passes LookPath: a chmod-0755
//     non-binary (ENOEXEC) — BeforeLaunch ran exactly once, proving
//     execution reached Start.
func TestNativeGraphProcessStartFailuresClassifiedNeverLaunched(t *testing.T) {
	requireNativeGraphAcceptance(t)
	dir := t.TempDir()

	notExec := filepath.Join(dir, "not-an-executable")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dirExec := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(dirExec, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{filepath.Join(dir, "does-not-exist"), notExec, dirExec} {
		b := newNativeGraphBackend(missing, nil)
		var launches atomic.Int32
		_, err := b.Execute(context.Background(), "hello", ExecOptions{
			NativeGraphProcess: true,
			BeforeLaunch:       func() error { launches.Add(1); return nil },
		})
		if !errors.Is(err, ErrNativeGraphNeverLaunched) {
			t.Fatalf("LookPath failure for %s not classified never-launched: %v", missing, err)
		}
		if n := launches.Load(); n != 0 {
			t.Fatalf("BeforeLaunch ran %d times for LookPath-rejected %s, want 0", n, missing)
		}
	}

	enoexec := filepath.Join(dir, "garbage-0755")
	if err := os.WriteFile(enoexec, []byte("not an elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := newNativeGraphBackend(enoexec, nil)
	var launches atomic.Int32
	_, err := b.Execute(context.Background(), "hello", ExecOptions{
		NativeGraphProcess: true,
		BeforeLaunch:       func() error { launches.Add(1); return nil },
	})
	if !errors.Is(err, ErrNativeGraphNeverLaunched) {
		t.Fatalf("Start ENOEXEC failure not classified never-launched: %v", err)
	}
	if n := launches.Load(); n != 1 {
		t.Fatalf("BeforeLaunch ran %d times for Start-failing ENOEXEC, want exactly 1", n)
	}
}

// TestNativeGraphProcessLegacyOptionsUnchanged: zero-value options keep the
// exact legacy error for a missing executable — no sentinel wrapping.
func TestNativeGraphProcessLegacyOptionsUnchanged(t *testing.T) {
	b := newNativeGraphBackend(filepath.Join(t.TempDir(), "does-not-exist"), nil)
	_, err := b.Execute(context.Background(), "hello", ExecOptions{})
	if err == nil {
		t.Fatal("expected legacy missing-executable error")
	}
	if errors.Is(err, ErrNativeGraphNeverLaunched) {
		t.Fatalf("legacy zero-option error must not carry the native sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "claude executable not found") {
		t.Fatalf("legacy error text changed: %v", err)
	}
}
