//go:build linux

package execenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// InitializeNativeSource initializes a freshly created, currently held native
// source domain with the Beads sandbox tool and publishes a private receipt.
// A failed attempt leaves partial init output in place (no reset, no
// adoption); the owner marker is validated unchanged afterwards. A valid
// receipt for the same manifest hash and executable replays without
// reinitializing.
//
// The child never resolves the source leaf by name: the pinned os.Root
// directory FD is inherited (Cmd.Dir and BEADS_DIR go through /proc/self/fd),
// so swapping the leaf between pin and exec cannot redirect writes.
func InitializeNativeSource(ctx context.Context, domain *NativeSourceDomain, executable string) error {
	if domain == nil || domain.Root == nil || domain.lock == nil {
		return errors.New("execenv: native source is not held")
	}
	if err := validateNativeSourceExecutable(executable); err != nil {
		return err
	}
	h, err := domain.ManifestHash()
	if err != nil {
		return err
	}
	if receipt, err := readNativeSourceReceipt(domain.Root); err != nil {
		return err
	} else if receipt != nil {
		if err := validateNativeSourceReceipt(receipt, h, executable); err != nil {
			return err
		}
		if err := nativeSourceBeadsDirOK(domain.Root); err != nil {
			return err
		}
		return nil
	}
	if err := nativeSourceDomainFresh(domain.Root); err != nil {
		return err
	}
	if err := nativeSourceVerifyVersion(ctx, executable, domain.Root); err != nil {
		return err
	}
	// The version child ran outside our control: re-verify the domain is
	// still fresh and the marker unchanged before creating anything.
	if err := nativeSourceDomainFresh(domain.Root); err != nil {
		return err
	}
	if after, err := domain.ManifestHash(); err != nil || after != h {
		return errors.New("execenv: native source owner marker changed during version check")
	}
	if err := domain.Root.Mkdir(nativeSourceHomeDir, 0o700); err != nil {
		return fmt.Errorf("execenv: create native source init home: %w", err)
	}
	if err := nativeSourceRunInit(ctx, executable, domain.Root); err != nil {
		return err
	}
	if err := nativeSourceBeadsDirOK(domain.Root); err != nil {
		return err
	}
	if after, err := domain.ManifestHash(); err != nil || after != h {
		return errors.New("execenv: native source owner marker changed during init")
	}
	return writeNativeSourceReceipt(domain.Root, h, executable)
}

const (
	nativeSourceInitBeadsVersion = "1.3.1"
	nativeSourceHomeDir          = ".native_init_home"
	nativeSourceBeadsDir         = ".beads"
	nativeSourceReceiptFile      = ".native_initialized"
	nativeSourceVersionMax       = 256
	nativeSourceInitBound        = 60 * time.Second
)

var nativeSourceInitArgv = []string{
	"--sandbox", "init", "--non-interactive", "--quiet",
	"--backend", "dolt", "--prefix", "multica",
	"--skip-agents", "--skip-hooks",
}

type nativeSourceInitReceipt struct {
	Version       string `json:"version"`
	ManifestHash  string `json:"manifest_hash"`
	Executable    string `json:"executable"`
	InitializedAt string `json:"initialized_at"`
}

// validateNativeSourceExecutable requires an explicit absolute, clean path.
func validateNativeSourceExecutable(executable string) error {
	if executable == "" || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return errors.New("execenv: native source executable must be an explicit absolute clean path")
	}
	return nil
}

// nativeSourceInitBaseEnv is the closed allowlist shared by version check and
// init. Nothing is inherited. PATH is derived solely from the selected
// executable's directory (the qualified Nix bd wrapper pins its trusted Dolt
// directory there); the invoking human's PATH is never inherited.
func nativeSourceInitBaseEnv(executable string) []string {
	return []string{
		"PATH=" + filepath.Dir(executable),
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"DO_NOT_TRACK=1",
		"DOLT_DISABLE_EVENT_FLUSH=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
}

func nativeSourceVersionEnv(executable string) []string {
	return append(nativeSourceInitBaseEnv(executable),
		"HOME=/proc/self/fd/3",
		// Same pinned BEADS_DIR as init, so the version check observes exactly
		// the environment init will run under (no ancestor/config discovery
		// mismatch).
		"BEADS_DIR=/proc/self/fd/3/"+nativeSourceBeadsDir,
	)
}

func nativeSourceInitEnv(executable string) []string {
	return append(nativeSourceInitBaseEnv(executable),
		"HOME=/proc/self/fd/3/"+nativeSourceHomeDir,
		"BEADS_DIR=/proc/self/fd/3/"+nativeSourceBeadsDir,
	)
}

// nativeSourceCommand pins the child to the held root. Go applies Cmd.Dir
// (chdir in the forked child) BEFORE the ExtraFiles FD mapping, so Dir uses
// the ORIGINAL parent FD number while env values use mapped child FD 3.
// WaitDelay bounds how long a descendant holding the output pipes can delay
// Run after the context kills the direct child; surviving descendants are
// NOT proven dead and no process-tree quiescence is claimed.
func nativeSourceCommand(ctx context.Context, executable string, root *os.Root, argv []string, env []string, stdout io.Writer) (*exec.Cmd, *os.File, error) {
	dirFD, err := root.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("execenv: pin native source exec dir: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable, argv...)
	cmd.WaitDelay = time.Second
	cmd.Dir = "/proc/self/fd/" + strconv.FormatUint(uint64(dirFD.Fd()), 10)
	cmd.ExtraFiles = []*os.File{dirFD}
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	return cmd, dirFD, nil
}

// boundedWriter caps captured output at max bytes; overflow is sticky.
type boundedWriter struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.overflow || w.buf.Len()+len(p) > w.max {
		w.overflow = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

// nativeSourceVerifyVersion runs the bounded --version check under the same
// pinned execution environment as init, before initialization.
func nativeSourceVerifyVersion(ctx context.Context, executable string, root *os.Root) error {
	out := &boundedWriter{max: nativeSourceVersionMax}
	cctx, cancel := context.WithTimeout(ctx, nativeSourceInitBound)
	defer cancel()
	cmd, dirFD, err := nativeSourceCommand(cctx, executable, root, []string{"--version"}, nativeSourceVersionEnv(executable), out)
	if err != nil {
		return err
	}
	defer dirFD.Close()
	if err := cmd.Run(); err != nil {
		if e := ctx.Err(); e != nil {
			return fmt.Errorf("execenv: native source executable version check failed: %w", e)
		}
		return fmt.Errorf("execenv: native source executable version check failed: %w", err)
	}
	if out.overflow || !nativeSourceVersionSupported(out.buf.String()) {
		// The raw output is not echoed: it is untrusted and may carry
		// unrelated or sensitive content.
		return fmt.Errorf("execenv: native source requires beads %s", nativeSourceInitBeadsVersion)
	}
	return nil
}

// nativeSourceVersionRe is the exact qualified grammar:
//
//	bd version 1.3.1 [optional dev annotation]
var nativeSourceVersionRe = regexp.MustCompile(`^bd version ` + regexp.QuoteMeta(nativeSourceInitBeadsVersion) + `( \([^)\n]{1,64}\))?$`)

func nativeSourceVersionSupported(out string) bool {
	return nativeSourceVersionRe.MatchString(strings.TrimSpace(out))
}

func nativeSourceRunInit(ctx context.Context, executable string, root *os.Root) error {
	cctx, cancel := context.WithTimeout(ctx, nativeSourceInitBound)
	defer cancel()
	cmd, dirFD, err := nativeSourceCommand(cctx, executable, root, nativeSourceInitArgv, nativeSourceInitEnv(executable), io.Discard)
	if err != nil {
		return err
	}
	defer dirFD.Close()
	// A real /dev/null fd (not io.Discard) so exec hands the child the fd
	// directly: no copying goroutine that a surviving descendant could block
	// by holding the pipe open.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("execenv: open devnull: %w", err)
	}
	defer devnull.Close()
	cmd.Stdout, cmd.Stderr = devnull, devnull
	if err := cmd.Run(); err != nil {
		if e := ctx.Err(); e != nil {
			return fmt.Errorf("execenv: native source init failed: %w", e)
		}
		return fmt.Errorf("execenv: native source init failed: %w", err)
	}
	return nil
}

// nativeSourceBeadsDirOK verifies through the pinned root that .beads is an
// actual directory, not a symlink or anything else.
func nativeSourceBeadsDirOK(root *os.Root) error {
	info, err := root.Lstat(nativeSourceBeadsDir)
	if err != nil {
		return fmt.Errorf("execenv: native source init produced no %s directory: %w", nativeSourceBeadsDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("execenv: native source %s must be a directory", nativeSourceBeadsDir)
	}
	return nil
}

// nativeSourceDomainFresh requires exactly the owner marker and lock. Any
// other entry, including a .native_init_home left by a torn attempt or a
// preexisting/symlinked .beads, refuses without reset or adoption. The read
// is bounded at 3: a fresh domain has exactly 2 entries.
func nativeSourceDomainFresh(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("execenv: inspect native source contents: %w", err)
	}
	defer f.Close()
	entries, err := f.ReadDir(3)
	if err != nil && err != io.EOF {
		return fmt.Errorf("execenv: read native source contents: %w", err)
	}
	if len(entries) != 2 {
		return errors.New("execenv: native source is not fresh; refusing to reset or adopt")
	}
	for _, e := range entries {
		if e.Name() != nativeSourceOwnerFile && e.Name() != nativeSourceLockFile {
			return fmt.Errorf("execenv: native source is not fresh (%q present); refusing to reset or adopt", e.Name())
		}
	}
	return nil
}

// writeNativeSourceReceipt publishes the private receipt with exclusive
// create, fsync of contents and of the directory entry.
func writeNativeSourceReceipt(root *os.Root, h, executable string) error {
	data, err := json.Marshal(nativeSourceInitReceipt{
		Version:       nativeSourceInitBeadsVersion,
		ManifestHash:  h,
		Executable:    executable,
		InitializedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	f, err := root.OpenFile(nativeSourceReceiptFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("execenv: create native source receipt: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("execenv: write native source receipt: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("execenv: sync native source receipt: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("execenv: close native source receipt: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("execenv: open native source for dir sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("execenv: sync native source directory: %w", err)
	}
	return nil
}

// readNativeSourceReceipt strictly reads the receipt through the pinned root,
// with the same opened-handle identity and bounded-read discipline as the
// owner marker. (nil, nil) only when absent.
func readNativeSourceReceipt(root *os.Root) (*nativeSourceInitReceipt, error) {
	lstat, err := root.Lstat(nativeSourceReceiptFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("execenv: inspect native source receipt: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 || !lstat.Mode().IsRegular() || lstat.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("execenv: native source receipt must be a private regular file")
	}
	f, err := root.OpenFile(nativeSourceReceiptFile, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("execenv: open native source receipt: %w", err)
	}
	defer f.Close()
	openStat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("execenv: stat native source receipt: %w", err)
	}
	if !os.SameFile(lstat, openStat) {
		return nil, errors.New("execenv: native source receipt changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, nativeSourceMarkerMax+1))
	if err != nil {
		return nil, fmt.Errorf("execenv: read native source receipt: %w", err)
	}
	if len(data) > nativeSourceMarkerMax {
		return nil, fmt.Errorf("execenv: native source receipt exceeds %d bytes", nativeSourceMarkerMax)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var r nativeSourceInitReceipt
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("execenv: decode native source receipt: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("execenv: trailing data after native source receipt")
	}
	if _, err := time.Parse(time.RFC3339, r.InitializedAt); err != nil {
		return nil, errors.New("execenv: native source receipt timestamp is not RFC3339")
	}
	return &r, nil
}

func validateNativeSourceReceipt(r *nativeSourceInitReceipt, h, executable string) error {
	if r.Version != nativeSourceInitBeadsVersion || r.ManifestHash != h || r.Executable != executable {
		return errors.New("execenv: native source receipt does not match this domain or executable; refusing")
	}
	return nil
}
