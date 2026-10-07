package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type nativePreparationManifest struct {
	ImportID       string `json:"import_id"`
	Provider       string `json:"provider"`
	SourceHandle   string `json:"source_handle"`
	SourceRevision string `json:"source_revision"`
	OwnedHandle    string `json:"owned_handle"`
	OwnedRevision  string `json:"owned_revision"`
	ResumeSession  string `json:"resume_session"`
	ResumeCwd      string `json:"resume_cwd"`
}

type nativePreparationIntent struct {
	ImportID       string `json:"import_id"`
	Provider       string `json:"provider"`
	SourceHandle   string `json:"source_handle"`
	SourceRevision string `json:"source_revision"`
	Phase          string `json:"phase"` // staged, cloning, or owned
	OwnedHandle    string `json:"owned_handle,omitempty"`
	OwnedRevision  string `json:"owned_revision,omitempty"`
	ResumeSession  string `json:"resume_session,omitempty"`
	ResumeCwd      string `json:"resume_cwd,omitempty"`
}

var nativePreparationLocks sync.Map // map[string]*sync.Mutex, keyed by manifest path

func withNativePreparationLock(path string, fn func() error) error {
	value, _ := nativePreparationLocks.LoadOrStore(path, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func nativePreparationPaths(opts NativeSessionPrepareOptions, provider string) (string, string, error) {
	if _, err := uuid.Parse(opts.ImportID); err != nil {
		return "", "", nativeSessionError(NativeSessionInvalidHistory, "import id must be a UUID")
	}
	if strings.TrimSpace(opts.Revision) == "" {
		return "", "", nativeSessionError(NativeSessionSourceChanged, "native preparation requires the selected revision")
	}
	if !filepath.IsAbs(opts.DestinationDir) {
		return "", "", nativeSessionError(NativeSessionInvalidHistory, "native preparation directory must be absolute")
	}
	destination := filepath.Clean(opts.DestinationDir)
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", "", nativeSessionError(NativeSessionInvalidHistory, "create native preparation directory: %v", err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return "", "", nativeSessionError(NativeSessionInvalidHistory, "protect native preparation directory: %v", err)
	}
	manifest := filepath.Join(destination, ".native-session-"+provider+"-"+opts.ImportID+".json")
	stage := filepath.Join(destination, ".native-stage-"+provider+"-"+opts.ImportID+".jsonl")
	return manifest, stage, nil
}

func readNativePreparationManifest(path string) (nativePreparationManifest, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nativePreparationManifest{}, false, nil
		}
		return nativePreparationManifest{}, false, nativeSessionError(NativeSessionInvalidHistory, "read native preparation manifest: %v", err)
	}
	var manifest nativePreparationManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.ImportID == "" || manifest.Provider == "" || manifest.OwnedHandle == "" || manifest.ResumeSession == "" {
		return nativePreparationManifest{}, false, nativeSessionError(NativeSessionInvalidHistory, "invalid native preparation manifest")
	}
	return manifest, true, nil
}

func writeNativePreparationManifest(path string, manifest nativePreparationManifest) error {
	if _, err := os.Lstat(path); err == nil {
		return nativeSessionError(NativeSessionInvalidHistory, "native preparation manifest already exists")
	} else if !os.IsNotExist(err) {
		return nativeSessionError(NativeSessionInvalidHistory, "inspect native preparation manifest: %v", err)
	}
	return writeNativeAtomicJSON(path, manifest)
}

func writeNativeAtomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "encode native preparation state: %v", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "create native preparation state: %v", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return nativeSessionError(NativeSessionInvalidHistory, "protect native preparation state: %v", err)
	}
	_, writeErr := temp.Write(append(data, '\n'))
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(temp.Name())
		return nativeSessionError(NativeSessionInvalidHistory, "write native preparation state: %v", writeErr)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		_ = os.Remove(temp.Name())
		return nativeSessionError(NativeSessionInvalidHistory, "publish native preparation state: %v", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "open native preparation directory: %v", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "sync native preparation directory: %v", syncErr)
	}
	if closeErr != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "close native preparation directory: %v", closeErr)
	}
	return nil
}

func nativePreparationIntentPath(stagePath string) string { return stagePath + ".intent" }

func readNativePreparationIntent(path string) (nativePreparationIntent, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nativePreparationIntent{}, false, nil
		}
		return nativePreparationIntent{}, false, nativeSessionError(NativeSessionInvalidHistory, "read native preparation intent: %v", err)
	}
	var intent nativePreparationIntent
	if err := json.Unmarshal(data, &intent); err != nil || intent.ImportID == "" || intent.Provider == "" {
		return nativePreparationIntent{}, false, nativeSessionError(NativeSessionInvalidHistory, "invalid native preparation intent")
	}
	return intent, true, nil
}

func nativeIntentMatches(intent nativePreparationIntent, opts NativeSessionPrepareOptions, provider string) bool {
	return intent.Provider == provider && intent.ImportID == opts.ImportID && intent.SourceHandle == opts.Handle && intent.SourceRevision == opts.Revision
}

func writeNativeStage(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "create native session staging copy: %v", err)
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return nativeSessionError(NativeSessionInvalidHistory, "write native session staging copy: %v", writeErr)
	}
	return nil
}

func readNativeStage(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > nativeSessionMaxBytes {
		return nil, nativeSessionError(NativeSessionResumeUnavailable, "invalid native session staging copy")
	}
	return os.ReadFile(path)
}

func nativeManifestMatches(manifest nativePreparationManifest, opts NativeSessionPrepareOptions, provider string) bool {
	return manifest.Provider == provider && manifest.ImportID == opts.ImportID && manifest.SourceHandle == opts.Handle && manifest.SourceRevision == opts.Revision && strings.TrimSpace(manifest.OwnedRevision) != ""
}
