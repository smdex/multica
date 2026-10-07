package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NativeSessionProvider is an optional read-only view of a provider's own
// persisted sessions. Handles are provider-private locators; callers must
// never expose them to a browser or use them as an execution resume pointer.
type NativeSessionProvider interface {
	ListNativeSessions(context.Context, NativeSessionListOptions) (NativeSessionPage, error)
	ReadNativeSession(context.Context, NativeSessionReadOptions) (NativeSessionSnapshot, error)
}

// NativeSessionImporter is deliberately separate from browsing. A successful
// preparation returns a distinct provider-owned fork which is the only resume
// identity an importer may schedule.
type NativeSessionImporter interface {
	PrepareNativeSession(context.Context, NativeSessionPrepareOptions) (PreparedNativeSession, error)
}

// NativeOwnedSessionProvider reads only a daemon-bound preparation directory.
// Its options must come from persisted daemon ownership, never browse input.
type NativeOwnedSessionProvider interface {
	ReadOwnedNativeSession(context.Context, NativeOwnedSessionReadOptions) (NativeSessionSnapshot, error)
}

type NativeOwnedSessionReadOptions struct {
	NativeSessionReadOptions
	DestinationDir string
	NativeID       string
}

type NativeSessionListOptions struct {
	Cursor string
	Limit  int
}

type NativeSessionReadOptions struct {
	Handle   string
	Revision string
}

type NativeSessionPrepareOptions struct {
	ImportID       string
	Handle         string
	Revision       string
	DestinationDir string
}

type NativeSessionSummary struct {
	NativeID  string
	Handle    string
	Revision  string
	Title     string
	Cwd       string
	Preview   string
	UpdatedAt time.Time
	Model     string
}

type NativeSessionPage struct {
	Sessions   []NativeSessionSummary
	NextCursor string
	Truncated  bool
}

type NativeHistoryMessage struct {
	NativeID  string
	Role      string
	Content   string
	CreatedAt time.Time
	Events    []Message
}

type NativeSessionSnapshot struct {
	Summary         NativeSessionSummary
	ResumeSessionID string
	Messages        []NativeHistoryMessage
	Warnings        []string
}

type PreparedNativeSession struct {
	Snapshot        NativeSessionSnapshot
	ResumeSessionID string
	ResumeCwd       string
}

// NativeSessionError carries the stable error code used across provider
// adapters. The wrapped error remains available to daemon logs without making
// provider payloads part of the public contract.
type NativeSessionError struct {
	Code string
	err  error
}

func (e *NativeSessionError) Error() string {
	if e.err == nil {
		return e.Code
	}
	return e.Code + ": " + e.err.Error()
}

func (e *NativeSessionError) Unwrap() error { return e.err }

const (
	NativeSessionUnsupported        = "unsupported"
	NativeSessionInvalidCursor      = "invalid_cursor"
	NativeSessionNotFound           = "not_found"
	NativeSessionSourceChanged      = "source_changed"
	NativeSessionBusy               = "session_busy"
	NativeSessionInvalidHistory     = "invalid_history"
	NativeSessionHistoryTooLarge    = "history_too_large"
	NativeSessionWorkdirUnavailable = "workdir_unavailable"
	NativeSessionResumeUnavailable  = "resume_unavailable"
)

func nativeSessionError(code, format string, args ...any) error {
	return &NativeSessionError{Code: code, err: fmt.Errorf(format, args...)}
}

func nativeSessionErrorCode(err error) string {
	var nativeErr *NativeSessionError
	if errors.As(err, &nativeErr) {
		return nativeErr.Code
	}
	return ""
}

const (
	nativeSessionDefaultLimit = 20
	nativeSessionMaxLimit     = 50
	nativeSessionMaxFiles     = 500
	nativeSessionMaxBytes     = 16 << 20
	nativeSessionMaxLineBytes = 1 << 20
)

func nativeSessionLimit(limit int) int {
	if limit <= 0 {
		return nativeSessionDefaultLimit
	}
	if limit > nativeSessionMaxLimit {
		return nativeSessionMaxLimit
	}
	return limit
}

func nativeSessionCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("v1:%d", offset)))
}

func parseNativeSessionCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, nativeSessionError(NativeSessionInvalidCursor, "malformed cursor")
	}
	var offset int
	if _, err := fmt.Sscanf(string(raw), "v1:%d", &offset); err != nil || offset < 0 {
		return 0, nativeSessionError(NativeSessionInvalidCursor, "malformed cursor")
	}
	return offset, nil
}

func nativeSessionIdentity(root, nativeID string) string {
	sum := sha256.Sum256([]byte(root))
	return nativeID + ":" + hex.EncodeToString(sum[:8])
}

func nativeSessionRevision(info os.FileInfo) string {
	// List operations deliberately use a stat-based revision so discovering a
	// row never reads the whole transcript. Read verifies both the opened
	// object and this revision before returning any snapshot.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", info.Size(), info.ModTime().UnixNano(), info.Mode())))
	return hex.EncodeToString(sum[:])
}

func nativeSessionTitle(provider, cwd, nativeID string) string {
	if cwd != "" {
		if base := filepath.Base(filepath.Clean(cwd)); base != "." && base != string(filepath.Separator) {
			return provider + " · " + base
		}
	}
	if len(nativeID) > 12 {
		nativeID = nativeID[:12]
	}
	return provider + " session " + nativeID
}

func effectiveNativeEnv(cfg Config, key string) string {
	// os/exec uses the last value when the merged environment repeats a key.
	env := buildEnv(cfg.Env)
	for i := len(env) - 1; i >= 0; i-- {
		name, value, ok := strings.Cut(env[i], "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func effectiveNativeHome(cfg Config) (string, error) {
	if home := strings.TrimSpace(effectiveNativeEnv(cfg, "HOME")); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nativeSessionError(NativeSessionWorkdirUnavailable, "resolve home: %v", err)
	}
	return home, nil
}

func canonicalNativeRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", nativeSessionError(NativeSessionInvalidHistory, "session root is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func nativePathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func nativeRootRelativePath(path string, roots []string) (string, string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", "", nativeSessionError(NativeSessionNotFound, "empty native handle")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", "", nativeSessionError(NativeSessionNotFound, "resolve native session path: %v", err)
	}
	absPath = filepath.Clean(absPath)
	for _, root := range roots {
		root = filepath.Clean(root)
		if nativePathWithinRoot(absPath, root) {
			rel, relErr := filepath.Rel(root, absPath)
			if relErr != nil || rel == "." || filepath.IsAbs(rel) {
				continue
			}
			// Do not resolve the selected path here. The rooted no-follow open
			// verifies every component; re-resolving a string after os.Open is
			// vulnerable to a symlink swap.
			return root, rel, filepath.Join(root, rel), nil
		}
	}
	return "", "", "", nativeSessionError(NativeSessionNotFound, "native session lies outside the configured roots")
}
