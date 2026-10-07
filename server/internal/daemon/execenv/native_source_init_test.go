//go:build linux

package execenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// nativeInitTool resolves an absolute path for a tool the fake script
// needs, at generation time, since the restricted child PATH cannot find it.
func nativeInitTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// writeNativeInitFakeExe writes a fake test-created executable. Tests never
// invoke any installed bd/agent CLI. The script logs its argv, working
// directory, resolved BEADS_DIR and its full environment export to an
// absolute log file baked in at creation, so nothing depends on PATH.
// Coreutils are baked in as absolute paths because the child's PATH only
// contains the executable's own directory.
func writeNativeInitFakeExe(t *testing.T, dir, name string, body []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	log := filepath.Join(dir, name+".log")
	lines := append([]string{
		"#!/bin/sh",
		`LOG="` + log + `"`,
		`echo "ARGV:$*" >> "$LOG"`,
		`echo "CWD:$(pwd)" >> "$LOG"`,
		`echo "BD:$BEADS_DIR" >> "$LOG"`,
		`export -p >> "$LOG"`,
	}, body...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// nativeInitFakeLog reads the fake executable's log lines.
func nativeInitFakeLog(t *testing.T, exe string) []string {
	t.Helper()
	b, err := os.ReadFile(exe + ".log")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func nativeInitFakeLaunches(t *testing.T, exe string) int {
	n := 0
	for _, l := range nativeInitFakeLog(t, exe) {
		if strings.HasPrefix(l, "ARGV:") {
			n++
		}
	}
	return n
}

// initFakeExeBody is the behaving 1.3.1 fake: --version then sandbox init.
func initFakeExeBody(t *testing.T) []string {
	t.Helper()
	return []string{
		`if [ "$1" = "--version" ]; then echo "bd version ` + nativeSourceInitBeadsVersion + `"; exit 0; fi`,
		`"` + nativeInitTool(t, "mkdir") + `" -p "$BEADS_DIR"`,
		`"` + nativeInitTool(t, "touch") + `" "$BEADS_DIR/db"`,
		`echo "BD-CREATED:$([ -d "$BEADS_DIR" ] && echo yes || echo no)" >> "$LOG"`,
		`"` + nativeInitTool(t, "sleep") + `" 0.1`,
	}
}

func newHeldNativeDomain(t *testing.T) (*NativeSourceDomain, string) {
	t.Helper()
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	d, err := CreateNativeSource(managed, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, filepath.Join(dir, id.SourceID)
}

func TestNativeInitSuccessArgvEnvAndPinnedRoot(t *testing.T) {
	// Hostile inherited variables must be scrubbed by the allowlist.
	t.Setenv("BEADS_DOLT_BACKEND", "hostile")
	t.Setenv("GIT_CONFIG", "/hostile/gitconfig")
	t.Setenv("BEADS_SANDBOX_HOOK", "/hostile/hook")
	t.Setenv("BEADS_AGENT_CONFIG", "/hostile/agent")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "hostile-cred")
	t.Setenv("SSH_AUTH_SOCK", "/hostile/agent.sock")

	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	if err := InitializeNativeSource(context.Background(), d, exe); err != nil {
		t.Fatal(err)
	}

	log := nativeInitFakeLog(t, exe)
	if len(log) < 6 {
		t.Fatalf("log too short: %q", log)
	}
	// The full environment export also lands in the log; every ARGV line is
	// deterministic so parsing by position is stable.
	var argvLines []string
	for _, l := range log {
		if strings.HasPrefix(l, "ARGV:") {
			argvLines = append(argvLines, l)
		}
	}
	if len(argvLines) != 2 || argvLines[0] != "ARGV:--version" {
		t.Fatalf("version argv: %q", argvLines)
	}
	wantArgv := "ARGV:" + strings.Join(nativeSourceInitArgv, " ")
	if argvLines[1] != wantArgv {
		t.Fatalf("init argv: got %q want %q", argvLines[1], wantArgv)
	}
	// Init must run inside the pinned held root and BEADS_DIR must resolve to
	// .beads inside it.
	var cwdLine, bdLine string
	var envLines []string
	for _, l := range log {
		switch {
		case strings.HasPrefix(l, "CWD:"):
			cwdLine = l
		case strings.HasPrefix(l, "BD:"):
			bdLine = l
		case strings.HasPrefix(l, "export "):
			envLines = append(envLines, l)
		}
	}
	if !strings.HasSuffix(cwdLine, filepath.Base(heldPath)) || strings.Contains(cwdLine, "..") {
		t.Fatalf("cwd not the held root: %q", cwdLine)
	}
	if !strings.HasSuffix(bdLine, "/"+nativeSourceBeadsDir) || bdLine != "BD:/proc/self/fd/3/"+nativeSourceBeadsDir {
		t.Fatalf("BEADS_DIR not pinned .beads: %q", bdLine)
	}
	var created string
	for _, l := range log {
		if strings.HasPrefix(l, "BD-CREATED:") {
			created = l
		}
	}
	if created != "BD-CREATED:yes" {
		t.Fatalf("init did not create pinned .beads: %q", created)
	}

	// Exact environment: the allowlist only, nothing inherited.
	var gotEnv []string
	for _, l := range envLines {
		l = strings.TrimPrefix(l, "export ")
		gotEnv = append(gotEnv, l)
	}
	// export -p may split values with spaces across lines; rejoin heuristics
	// are fragile, so compare key/value pairs of interest and total count of
	// exported keys against the allowlist keys.
	allow := nativeSourceInitEnv(exe)
	allowKeys := map[string]bool{}
	for _, kv := range allow {
		allowKeys[strings.SplitN(kv, "=", 2)[0]] = true
	}
	// The child shell itself sets these after chdir; they are not inherited
	// from the parent environment.
	for _, k := range []string{"PWD", "OLDPWD", "SHLVL", "IFS", "OPTIND", "PS1", "PS2", "PS4"} {
		allowKeys[k] = true
	}
	for _, kv := range gotEnv {
		key := strings.SplitN(kv, "=", 2)[0]
		if !allowKeys[key] {
			t.Fatalf("environment key %q not in allowlist (hostile env leaked)", key)
		}
	}
	for _, hostile := range []string{"hostile", "SECRET", "AWS_"} {
		for _, kv := range gotEnv {
			if strings.Contains(kv, hostile) {
				t.Fatalf("hostile value leaked into environment: %q", kv)
			}
		}
	}
	// PATH must be the executable's directory only, never the inherited PATH.
	// `export -p` quotes values, so strip shell quoting before comparing.
	unquote := func(s string) string {
		if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
			return s[1 : len(s)-1]
		}
		return s
	}
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "PATH=") && unquote(strings.TrimPrefix(kv, "PATH=")) != exeDir {
			t.Fatalf("PATH not pinned to executable dir: %q", kv)
		}
	}

	// Receipt: private, regular, valid.
	info, err := os.Lstat(filepath.Join(heldPath, nativeSourceReceiptFile))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt wrong: %+v %v", info, err)
	}
	var receipt nativeSourceInitReceipt
	if err := json.Unmarshal([]byte(mustRead(t, filepath.Join(heldPath, nativeSourceReceiptFile))), &receipt); err != nil {
		t.Fatal(err)
	}
	h, err := d.ManifestHash()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Version != nativeSourceInitBeadsVersion || receipt.ManifestHash != h || receipt.Executable != exe {
		t.Fatalf("receipt mismatch: %+v", receipt)
	}
	if _, err := time.Parse(time.RFC3339, receipt.InitializedAt); err != nil {
		t.Fatalf("receipt timestamp: %v", err)
	}
}

func TestNativeInitReplayExactlyOneInitLaunch(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, _ := newHeldNativeDomain(t)
	ctx := context.Background()
	if err := InitializeNativeSource(ctx, d, exe); err != nil {
		t.Fatal(err)
	}
	if n := nativeInitFakeLaunches(t, exe); n != 2 {
		t.Fatalf("first init launches = %d, want 2 (version+init)", n)
	}
	for i := 0; i < 3; i++ {
		if err := InitializeNativeSource(ctx, d, exe); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	if n := nativeInitFakeLaunches(t, exe); n != 2 {
		t.Fatalf("replay reinitialized: launches = %d, want 2", n)
	}
}

func TestNativeInitRejectsBadExecutableAndUngeldDomain(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, _ := newHeldNativeDomain(t)
	ctx := context.Background()
	if err := InitializeNativeSource(ctx, d, "bd"); err == nil {
		t.Fatal("relative executable accepted")
	}
	// An unclean absolute path that survives as text: raw concatenation,
	// not filepath.Join (which normalizes "/../" away).
	unclean := exeDir + "/../" + filepath.Base(exe)
	if filepath.Clean(unclean) == unclean {
		t.Fatalf("test bug: %q is already clean", unclean)
	}
	if err := InitializeNativeSource(ctx, d, unclean); err == nil {
		t.Fatal("unclean executable accepted")
	}
	if err := InitializeNativeSource(ctx, nil, exe); err == nil {
		t.Fatal("nil domain accepted")
	}
	d.Close()
	if err := InitializeNativeSource(ctx, d, exe); err == nil {
		t.Fatal("closed domain accepted")
	}
	if n := nativeInitFakeLaunches(t, exe); n != 0 {
		t.Fatal("rejection launched the executable")
	}
}

func TestNativeInitUnknownVersionNoInit(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", []string{
		`echo "beads 9.9.9"`,
	})
	d, heldPath := newHeldNativeDomain(t)
	if err := InitializeNativeSource(context.Background(), d, exe); err == nil {
		t.Fatal("unknown version accepted")
	}
	if n := nativeInitFakeLaunches(t, exe); n != 1 {
		t.Fatalf("unknown version must stop after version check: %d launches", n)
	}
	if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceBeadsDir)); !os.IsNotExist(err) {
		t.Fatal("unknown version initialized .beads")
	}
	if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceReceiptFile)); !os.IsNotExist(err) {
		t.Fatal("unknown version published a receipt")
	}
	if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceHomeDir)); !os.IsNotExist(err) {
		t.Fatal("unknown version created the init home")
	}
}

func TestNativeInitNonemptyDomainRefused(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	junk := filepath.Join(heldPath, "existing-user-data.txt")
	if err := os.WriteFile(junk, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(context.Background(), d, exe); err == nil {
		t.Fatal("nonempty domain initialized")
	}
	if got := mustRead(t, junk); got != "precious" {
		t.Fatalf("existing data altered: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceReceiptFile)); !os.IsNotExist(err) {
		t.Fatal("refused path published a receipt")
	}
	if n := nativeInitFakeLaunches(t, exe); n != 0 {
		t.Fatal("refused path launched the executable")
	}
}

func TestNativeInitPreexistingBeadsSymlinkRefused(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(heldPath, nativeSourceBeadsDir)); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(context.Background(), d, exe); err == nil {
		t.Fatal("preexisting .beads symlink adopted")
	}
	if fi, err := os.Lstat(filepath.Join(heldPath, nativeSourceBeadsDir)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink .beads altered: %+v %v", fi, err)
	}
	if n := nativeInitFakeLaunches(t, exe); n != 0 {
		t.Fatal("symlink refusal launched the executable")
	}
}

func TestNativeInitLeafSwappedPinnedRootPreserved(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	swapped := heldPath + ".swapped-in"
	if err := os.Mkdir(swapped, 0o700); err != nil {
		t.Fatal(err)
	}
	honey := filepath.Join(swapped, "honeypot.txt")
	_ = honey
	if err := os.WriteFile(honey, []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	// While init runs, swap the leaf name: rename the held directory away and
	// put the decoy under the original name. Use a background swap once the
	// fake has started (it sleeps 0.1s).
	done := make(chan error, 1)
	var swappedPath string
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		log := exe + ".log"
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(log); err == nil && strings.Contains(string(b), "ARGV:--sandbox") {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Swap by content, not rename: move the decoy into the pinned
		// directory's original name is impossible while it is held, so rename
		// the pinned dir aside and move the decoy under the old name.
		if err := os.Rename(heldPath, heldPath+".original"); err != nil {
			done <- err
			return
		}
		swappedPath = swapped
		swapped = "" // consumed
		done <- os.Rename(swappedPath, heldPath)
	}()
	if err := InitializeNativeSource(context.Background(), d, exe); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Success state must live under the ORIGINAL held (pinned) directory,
	// not the decoy now sitting under the original leaf name.
	if got := mustRead(t, filepath.Join(heldPath, "honeypot.txt")); got != "decoy" {
		t.Fatalf("decoy content altered: %q", got)
	}
	for _, name := range []string{nativeSourceBeadsDir, nativeSourceReceiptFile, nativeSourceHomeDir} {
		if _, err := os.Lstat(filepath.Join(heldPath, name)); !os.IsNotExist(err) {
			t.Fatalf("swapped-in decoy received %q", name)
		}
	}
	orig := heldPath + ".original"
	for _, name := range []string{nativeSourceBeadsDir, nativeSourceReceiptFile, nativeSourceHomeDir} {
		if _, err := os.Lstat(filepath.Join(orig, name)); err != nil {
			t.Fatalf("pinned original missing %q: %v", name, err)
		}
	}
	// Replay still succeeds against the pinned root.
	if err := InitializeNativeSource(context.Background(), d, exe); err != nil {
		t.Fatalf("replay after swap: %v", err)
	}
}

func TestNativeInitVersionGrammarExact(t *testing.T) {
	ok := []string{
		"bd version 1.3.1",
		"bd version 1.3.1 (dev)",
		"bd version 1.3.1 \n",
	}
	bad := []string{
		"beads 1.3.1",
		"not bd 1.3.1",
		"bd version 1.3.2",
		"bd version 1.3.1 extra",
		"bd version 1.3.1\nother output",
		"",
	}
	for _, s := range ok {
		if !nativeSourceVersionSupported(s) {
			t.Fatalf("rejected valid version output %q", s)
		}
	}
	for _, s := range bad {
		if nativeSourceVersionSupported(s) {
			t.Fatalf("accepted invalid version output %q", s)
		}
	}
}

func TestNativeInitTornHomeRefusedBeforeLaunch(t *testing.T) {
	// A .native_init_home without a receipt is torn state from a previous
	// attempt; it must refuse before launching anything.
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	if err := os.Mkdir(filepath.Join(heldPath, nativeSourceHomeDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(context.Background(), d, exe); err == nil {
		t.Fatal("torn init home accepted")
	}
	if n := nativeInitFakeLaunches(t, exe); n != 0 {
		t.Fatal("torn state launched the executable")
	}
	if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceHomeDir)); err != nil {
		t.Fatal("torn state was cleaned up")
	}
}

func TestNativeInitVersionOverflowStickyReject(t *testing.T) {
	// Even a matching version buried in over-long output must be rejected,
	// and the error must not echo the raw output.
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", []string{
		`echo "SECRETTOKEN bd version 1.3.1 $(head -c 500 /dev/zero | tr '\0' 'x')"`,
	})
	d, _ := newHeldNativeDomain(t)
	err := InitializeNativeSource(context.Background(), d, exe)
	if err == nil || !strings.Contains(err.Error(), "1.3.1") || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("overflow output handling wrong: %v", err)
	}
}

func TestNativeInitReceiptMismatchesRefused(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	exe2 := writeNativeInitFakeExe(t, exeDir, "fake-bd-2", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	ctx := context.Background()
	if err := InitializeNativeSource(ctx, d, exe); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(heldPath, nativeSourceReceiptFile)
	valid := mustRead(t, receiptPath)

	if err := InitializeNativeSource(ctx, d, exe2); err == nil {
		t.Fatal("different executable replay accepted")
	}
	for name, body := range map[string]string{
		"truncated":  valid[:len(valid)/2],
		"garbage":    "not json",
		"trailing":   valid + " x",
		"wrong-time": strings.Replace(valid, `"initialized_at":"`, `"initialized_at":"nope`, 1),
	} {
		if err := os.WriteFile(receiptPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := InitializeNativeSource(ctx, d, exe); err == nil {
			t.Fatalf("%s receipt accepted", name)
		}
	}
	// A torn (empty) receipt refuses.
	if err := os.WriteFile(receiptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(ctx, d, exe); err == nil {
		t.Fatal("empty receipt accepted")
	}
	// Restoring the valid receipt makes replay work again.
	if err := os.WriteFile(receiptPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(ctx, d, exe); err != nil {
		t.Fatalf("valid receipt replay: %v", err)
	}
	// A receipt symlink is refused outright.
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(exeDir, "elsewhere"), receiptPath); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(ctx, d, exe); err == nil {
		t.Fatal("receipt symlink accepted")
	}
}

func TestNativeInitReplayRequiresBeadsDir(t *testing.T) {
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "fake-bd", initFakeExeBody(t))
	d, heldPath := newHeldNativeDomain(t)
	ctx := context.Background()
	if err := InitializeNativeSource(ctx, d, exe); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(heldPath, nativeSourceBeadsDir)); err != nil {
		t.Fatal(err)
	}
	if err := InitializeNativeSource(ctx, d, exe); err == nil {
		t.Fatal("replay accepted without .beads")
	}
	if n := nativeInitFakeLaunches(t, exe); n != 2 {
		t.Fatal("failed replay reinitialized")
	}
}

func TestNativeInitVersionDescendantHoldingPipeBounded(t *testing.T) {
	// Regression: the version check captures output through a pipe. If the
	// direct child exits but a descendant keeps the pipe open, the parent's
	// context deadline plus WaitDelay must still return promptly (within 2s
	// of a 25ms deadline), instead of blocking on the copying goroutine.
	exeDir := t.TempDir()
	exe := writeNativeInitFakeExe(t, exeDir, "pipe-holder", []string{
		`if [ "$1" = "--version" ]; then echo "bd version ` + nativeSourceInitBeadsVersion + `"; fi`,
		`"` + nativeInitTool(t, "sleep") + `" 300 &`,
		`exit 0`,
	})
	d, _ := newHeldNativeDomain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- InitializeNativeSource(ctx, d, exe) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected deadline error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("descendant holding the version pipe delayed return beyond 2s")
	}
}

func TestNativeInitVersionSideEffectsRefused(t *testing.T) {
	// A version child that writes into the domain (or mutates the marker)
	// must not be adopted: no init call runs and partial output is left in
	// place, refused.
	t.Run("creates-content", func(t *testing.T) {
		exeDir := t.TempDir()
		exe := writeNativeInitFakeExe(t, exeDir, "writer-bd", append([]string{
			// Side effect first, then a VALID version line: the rejection must
			// come from the post-version freshness fence, not the grammar check.
			`if [ "$1" = "--version" ]; then ` + nativeInitTool(t, "mkdir") + ` -p "$BEADS_DIR" && echo "bd version 1.3.1 (dev)" && exit 0; fi`,
		}, initFakeExeBody(t)...))
		d, heldPath := newHeldNativeDomain(t)
		err := InitializeNativeSource(context.Background(), d, exe)
		if err == nil {
			t.Fatal("version side effect adopted")
		}
		if n := nativeInitFakeLaunches(t, exe); n != 1 {
			t.Fatalf("init ran after version side effect: %d launches", n)
		}
		if !strings.Contains(err.Error(), "not fresh") {
			t.Fatalf("rejection did not come from the freshness fence: %v", err)
		}
		if _, statErr := os.Lstat(filepath.Join(heldPath, nativeSourceBeadsDir)); statErr != nil {
			t.Fatalf("partial version output was cleaned up: %v", statErr)
		}
		if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceReceiptFile)); !os.IsNotExist(err) {
			t.Fatal("side-effect path published a receipt")
		}
	})
	t.Run("changes-marker", func(t *testing.T) {
		exeDir := t.TempDir()
		exe := writeNativeInitFakeExe(t, exeDir, "marker-bd", append([]string{
			`if [ "$1" = "--version" ]; then printf '{}' > /proc/self/fd/3/` + nativeSourceOwnerFile + ` && echo "bd version 1.3.1 (dev)" && exit 0; fi`,
		}, initFakeExeBody(t)...))
		d, heldPath := newHeldNativeDomain(t)
		err := InitializeNativeSource(context.Background(), d, exe)
		if err == nil {
			t.Fatal("marker mutation during version check adopted")
		}
		if n := nativeInitFakeLaunches(t, exe); n != 1 {
			t.Fatalf("init ran after marker mutation: %d launches", n)
		}
		if !strings.Contains(err.Error(), "marker changed") {
			t.Fatalf("rejection did not come from the marker fence: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceReceiptFile)); !os.IsNotExist(err) {
			t.Fatal("mutated-marker path published a receipt")
		}
	})
}

func TestNativeInitContextCancelAndTimeoutNoReceipt(t *testing.T) {
	exeDir := t.TempDir()
	slow := writeNativeInitFakeExe(t, exeDir, "slow-bd", []string{
		`if [ "$1" = "--version" ]; then echo "bd version ` + nativeSourceInitBeadsVersion + `"; exit 0; fi`,
		`"` + nativeInitTool(t, "sleep") + `" 30`,
	})
	t.Run("cancel", func(t *testing.T) {
		d, heldPath := newHeldNativeDomain(t)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if n := nativeInitFakeLaunches(t, slow); n >= 2 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		if err := InitializeNativeSource(ctx, d, slow); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
		assertNoNativeInitReceipt(t, heldPath)
	})
	// The caller's context deadline exercises the same bound without waiting
	// 60 seconds.
	t.Run("timeout", func(t *testing.T) {
		d, heldPath := newHeldNativeDomain(t)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if err := InitializeNativeSource(ctx, d, slow); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected timeout, got %v", err)
		}
		assertNoNativeInitReceipt(t, heldPath)
	})
}

func assertNoNativeInitReceipt(t *testing.T, heldPath string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(heldPath, nativeSourceReceiptFile)); !os.IsNotExist(err) {
		t.Fatal("failed init published a receipt")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNativeInitVersionEnvIsClosedAllowlist(t *testing.T) {
	// The version check environment must carry HOME pinned to the held root
	// (FD 3), telemetry disabled, and no inherited variables.
	exe := "/opt/trusted/bd"
	env := nativeSourceVersionEnv(exe)
	want := []string{
		"PATH=/opt/trusted",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"DO_NOT_TRACK=1",
		"DOLT_DISABLE_EVENT_FLUSH=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=/proc/self/fd/3",
		"BEADS_DIR=/proc/self/fd/3/" + nativeSourceBeadsDir,
	}
	sort.Strings(env)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if strings.Join(env, "\x00") != strings.Join(sortedWant, "\x00") {
		t.Fatalf("version env mismatch:\n got %v\nwant %v", env, sortedWant)
	}
	initEnv := nativeSourceInitEnv(exe)
	joined := strings.Join(initEnv, "\n")
	if !strings.Contains(joined, "HOME=/proc/self/fd/3/"+nativeSourceHomeDir) ||
		!strings.Contains(joined, "BEADS_DIR=/proc/self/fd/3/"+nativeSourceBeadsDir) {
		t.Fatalf("init env missing pinned HOME/BEADS_DIR: %v", initEnv)
	}
	if len(initEnv) != len(want) { // init env = base + HOME + BEADS_DIR
		t.Fatalf("init env has unexpected extras: %v", initEnv)
	}
}

func TestNativeInitCommandDirUsesOriginalFDBeforeMapping(t *testing.T) {
	// Cmd.Dir must use the parent-process FD number (Go chdirs before the
	// ExtraFiles remap), while the environment uses child FD 3.
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	cmd, dirFD, err := nativeSourceCommand(context.Background(), "/bin/true", root, nil, nativeSourceVersionEnv("/bin/true"), ioDiscard())
	if err != nil {
		t.Fatal(err)
	}
	defer dirFD.Close()
	if cmd.Dir != fmt.Sprintf("/proc/self/fd/%d", dirFD.Fd()) {
		t.Fatalf("Dir uses mapped FD: %q (fd %d)", cmd.Dir, dirFD.Fd())
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != dirFD {
		t.Fatal("pinned root FD not the single extra file")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "HOME=") && kv != "HOME=/proc/self/fd/3" {
			t.Fatalf("env HOME not mapped child FD 3: %q", kv)
		}
	}
}

func ioDiscard() (w interface{ Write([]byte) (int, error) }) { return devNullWriter{} }

type devNullWriter struct{}

func (devNullWriter) Write(p []byte) (int, error) { return len(p), nil }
