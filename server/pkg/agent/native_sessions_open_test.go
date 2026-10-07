//go:build linux || darwin

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativeOpenRootRejectsSymlinkAncestorAndFIFO(t *testing.T) {
	parent := t.TempDir()
	actual := filepath.Join(parent, "actual")
	root := filepath.Join(actual, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if file, err := nativeOpenRootFile(filepath.Join(link, "root"), "session.jsonl"); err == nil {
		_ = file.Close()
		t.Fatal("root acquisition followed a symlink ancestor")
	}

	fifo := filepath.Join(root, "blocked.jsonl")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readNativeFile(fifo, []string{root}, "")
		done <- err
	}()
	select {
	case err := <-done:
		if nativeSessionErrorCode(err) != NativeSessionNotFound {
			t.Fatalf("FIFO error = %v (%s)", err, nativeSessionErrorCode(err))
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("FIFO blocked native session validation")
	}
}
