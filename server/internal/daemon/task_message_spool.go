package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/redact"
)

const (
	taskMessageSpoolVersion       = 1
	taskMessageSpoolMaxBytes      = 64 << 20
	taskMessageSpoolMaxFiles      = 1024
	taskMessageSpoolMaxBatchBytes = 8 << 20
)

// ponytail: quota scans and writes serialize within this daemon and scan at
// most 1024 retained files per namespace. A shared root used by multiple daemon
// processes needs a cross-process quota lock before claiming an aggregate cap.
var taskMessageSpoolMu sync.Mutex

// capturedTaskMessageBatch is local evidence, NOT a server replay receipt.
// LegacyOneShot records were sent without a server dedupe key. Even after a
// server upgrade, they must not be automatically resent: the first request may
// already have committed. CaptureID identifies this local Execute call, not a
// fabricated server incarnation. IncarnationID is present only when known.
// LegacyOneShot=false marks an identified batch whose exact payload+BatchID
// may be resent while this execution lives, because the server dedupes exact
// replays and rejects a changed payload under the same BatchID with a 409.
// No credential is stored. Unix modes are private, Windows relies on the
// configured workspace root's user ACL, as the existing terminal outbox does.
// Directory entries are fsynced on Unix. Windows flushes the file only, so
// crash-safe retention of its directory entry is not qualified by this slice.
// Retained failures have no automatic expiry or eviction, so reaching the cap
// explicitly fails capture rather than deleting unacknowledged evidence.
type capturedTaskMessageBatch struct {
	Version       int               `json:"version"`
	TaskID        string            `json:"task_id"`
	CaptureID     string            `json:"capture_id"`
	IncarnationID string            `json:"incarnation_id,omitempty"`
	BatchID       string            `json:"batch_id"`
	LegacyOneShot bool              `json:"legacy_one_shot"`
	CapturedAt    time.Time         `json:"captured_at"`
	Digest        string            `json:"digest"`
	Messages      []TaskMessageData `json:"messages"`
}

type taskMessageSpool struct {
	dir           string
	taskID        string
	captureID     string
	incarnationID string
	legacyOneShot bool
}

func newTaskMessageSpool(cfg Config, taskID, incarnationID string, legacyOneShot bool) *taskMessageSpool {
	// Reuse the terminal outbox's exact server/profile/daemon namespace so a
	// reconnect to another deployment can never adopt this capture by accident.
	terminalStore := newTerminalReportStore(cfg)
	if terminalStore == nil {
		return nil
	}
	return &taskMessageSpool{
		dir: filepath.Join(cfg.WorkspacesRoot, ".task-message-spool", "v1", terminalStore.namespace),
		taskID: taskID, captureID: uuid.NewString(), incarnationID: incarnationID, legacyOneShot: legacyOneShot,
	}
}

func sanitizeTaskMessageBatch(messages []TaskMessageData) []TaskMessageData {
	cleaned := slices.Clone(messages)
	for i := range cleaned {
		m := &cleaned[i]
		m.Type = util.SanitizeTextForPostgres(redact.Text(m.Type))
		m.Tool = util.SanitizeTextForPostgres(redact.Text(m.Tool))
		m.CallID = util.SanitizeTextForPostgres(redact.Text(m.CallID))
		m.Content = util.SanitizeTextForPostgres(redact.Text(m.Content))
		m.Output = util.SanitizeTextForPostgres(redact.Text(m.Output))
		if m.Input != nil {
			m.Input, _ = util.SanitizeJSONForPostgres(redact.InputMap(m.Input)).(map[string]any)
		}
	}
	return cleaned
}

func (record capturedTaskMessageBatch) digest() (string, error) {
	record.Digest = ""
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func (s *taskMessageSpool) capture(messages []TaskMessageData) (capturedTaskMessageBatch, error) {
	record := capturedTaskMessageBatch{
		Version: taskMessageSpoolVersion, TaskID: s.taskID, CaptureID: s.captureID,
		IncarnationID: s.incarnationID, BatchID: uuid.NewString(), LegacyOneShot: s.legacyOneShot,
		CapturedAt: time.Now().UTC(),
	}
	if strings.TrimSpace(s.taskID) == "" || len(messages) == 0 {
		return record, errors.New("task message capture requires task identity and messages")
	}
	for i, message := range messages {
		if message.Seq <= 0 || (i > 0 && message.Seq <= messages[i-1].Seq) {
			return record, errors.New("task message capture sequences must be positive and increasing")
		}
	}
	// Fast admission ceiling on the RAW encoded size, before the expensive
	// redaction/hash pipeline. Redaction can shrink a body substantially, so
	// this is a conservative bound, not a proof the sanitized body fits: a raw
	// batch already past the limit is rejected without paying for redaction,
	// while batches under it still face the exact post-sanitize check below.
	raw, rawErr := json.Marshal(messages)
	if rawErr != nil {
		return record, fmt.Errorf("encode task message capture preflight: %w", rawErr)
	}
	if len(raw) > taskMessageSpoolMaxBatchBytes {
		return record, errors.New("task message capture exceeds batch byte limit")
	}
	record.Messages = sanitizeTaskMessageBatch(messages)
	var err error
	record.Digest, err = record.digest()
	if err != nil {
		return record, fmt.Errorf("hash task message capture: %w", err)
	}
	body, err := json.Marshal(record)
	if err != nil {
		return record, fmt.Errorf("encode task message capture: %w", err)
	}
	if len(body) > taskMessageSpoolMaxBatchBytes {
		return record, errors.New("task message capture exceeds batch byte limit")
	}
	taskMessageSpoolMu.Lock()
	defer taskMessageSpoolMu.Unlock()
	// Tighten/check every managed ancestor, not just the leaf. The operator's
	// workspaces root can itself be a symlink, but our private children cannot.
	for _, dir := range []string{filepath.Dir(filepath.Dir(s.dir)), filepath.Dir(s.dir), s.dir} {
		_, priorErr := os.Lstat(dir)
		if err := ensureTerminalReportDir(dir); err != nil {
			return record, fmt.Errorf("secure task message spool: %w", err)
		}
		if errors.Is(priorErr, os.ErrNotExist) {
			if err := syncTerminalReportDir(filepath.Dir(dir)); err != nil {
				return record, fmt.Errorf("sync new task message spool directory: %w", err)
			}
		}
	}
	count, bytes, err := terminalReportDirectoryStats(s.dir)
	if err != nil {
		return record, fmt.Errorf("inspect task message spool quota: %w", err)
	}
	if count >= taskMessageSpoolMaxFiles || bytes+int64(len(body)) > taskMessageSpoolMaxBytes {
		return record, errors.New("task message spool is full; unacknowledged captures retained")
	}
	// A partial temp file remains evidence of an interrupted capture. It is
	// counted against quota and never treated as a complete batch or replayed.
	tmp, err := os.CreateTemp(s.dir, "."+record.BatchID+"-*.tmp")
	if err != nil {
		return record, fmt.Errorf("create task message capture: %w", err)
	}
	defer tmp.Close()
	if _, err := tmp.Write(body); err != nil {
		return record, fmt.Errorf("write task message capture: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return record, fmt.Errorf("sync task message capture: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return record, fmt.Errorf("close task message capture: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, record.BatchID+".json")); err != nil {
		return record, fmt.Errorf("publish task message capture: %w", err)
	}
	if err := syncTerminalReportDir(s.dir); err != nil {
		return record, fmt.Errorf("sync task message spool: %w", err)
	}
	return record, nil
}

// acknowledge is called only after this process's send was proven committed:
// a legacy one-shot success, or an identified batch's exact receipt echo.
// It is not a retry API and cannot establish a contiguous server high-water.
func (s *taskMessageSpool) acknowledge(record capturedTaskMessageBatch) error {
	if record.TaskID != s.taskID || record.CaptureID != s.captureID {
		return errors.New("task message acknowledgement does not match capture identity")
	}
	if _, err := uuid.Parse(record.BatchID); err != nil {
		return errors.New("task message acknowledgement has invalid batch identity")
	}
	taskMessageSpoolMu.Lock()
	defer taskMessageSpoolMu.Unlock()
	path := filepath.Join(s.dir, record.BatchID+".json")
	persisted, err := readCapturedTaskMessageBatch(path)
	if err != nil {
		return fmt.Errorf("validate acknowledged task message capture: %w", err)
	}
	want, err := record.digest()
	if err != nil || want != record.Digest || persisted.Digest != record.Digest {
		return errors.New("task message acknowledgement does not match persisted payload")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove acknowledged task message capture: %w", err)
	}
	return syncTerminalReportDir(s.dir)
}

// readCapturedTaskMessageBatch supports offline validation only. There is
// deliberately no startup loader or cross-restart replay; the in-process
// bounded exact-payload retry lives in Client.ReportTaskMessageBatch.
func readCapturedTaskMessageBatch(path string) (capturedTaskMessageBatch, error) {
	var record capturedTaskMessageBatch
	info, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Size() > taskMessageSpoolMaxBatchBytes {
		return record, errors.New("task message capture is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, taskMessageSpoolMaxBatchBytes+1))
	if err != nil || len(body) > taskMessageSpoolMaxBatchBytes {
		return record, errors.New("task message capture could not be read within byte limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("decode task message capture: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, errors.New("task message capture has trailing data")
	}
	if record.Version != taskMessageSpoolVersion || record.TaskID == "" || record.CaptureID == "" ||
		len(record.Messages) == 0 || filepath.Base(path) != record.BatchID+".json" {
		return record, errors.New("invalid task message capture identity/version/delivery mode")
	}
	want, err := record.digest()
	if err != nil || want != record.Digest {
		return record, errors.New("task message capture digest mismatch")
	}
	return record, nil
}
