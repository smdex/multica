package execenv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNativeSourceManifestHash(t *testing.T) {
	managed, _ := openManagedRoot(t)
	id := testIdentity(testSourceID())
	d, err := CreateNativeSource(managed, id)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	marker, err := d.Root.ReadFile(nativeSourceOwnerFile)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(marker)
	got, err := d.ManifestHash()
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("manifest hash mismatch: %v", err)
	}
	if err := d.Root.WriteFile(nativeSourceOwnerFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ManifestHash(); err == nil {
		t.Fatal("changed marker accepted")
	}
	d.Close()
	if _, err := d.ManifestHash(); err == nil {
		t.Fatal("closed domain accepted")
	}
}

func TestNativeSourceReopenRejectsPublicPermissions(t *testing.T) {
	for _, leaf := range []string{"", nativeSourceLockFile, nativeSourceOwnerFile} {
		t.Run(leaf, func(t *testing.T) {
			managed, dir := openManagedRoot(t)
			id := testIdentity(testSourceID())
			d, err := CreateNativeSource(managed, id)
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
			path := filepath.Join(dir, id.SourceID, leaf)
			mode := os.FileMode(0o644)
			if leaf == "" {
				mode = 0o755
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if d, err := OpenNativeSource(managed, id); err == nil {
				d.Close()
				t.Fatal("public native domain state reopened")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("rejection changed existing permissions or data")
			}
		})
	}
}

func testIdentity(sourceID string) NativeSourceIdentity {
	return NativeSourceIdentity{
		BackendURL:   "https://backend.example.com",
		WorkspaceID:  "11111111-1111-1111-1111-111111111111",
		SourceID:     sourceID,
		RuntimeID:    "22222222-2222-2222-2222-222222222222",
		DaemonID:     "daemon-1",
		EnrollmentID: "33333333-3333-3333-3333-333333333333",
	}
}

func testSourceID() string { return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" }

func TestNativeSourceBackendURLRejectsAliasesWithoutDisclosure(t *testing.T) {
	for _, raw := range []string{"https://backend.example.com?", "https://backend.example.com#fragment", "https://operator:SECRET@backend.example.com", "https://operator:SECRET@%"} {
		t.Run(raw, func(t *testing.T) {
			managed, _ := openManagedRoot(t)
			id := testIdentity(testSourceID())
			id.BackendURL = raw
			_, err := CreateNativeSource(managed, id)
			if err == nil {
				t.Fatal("ambiguous or credential-bearing backend URL accepted")
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("backend URL error disclosed credentials")
			}
			if _, err := managed.Lstat(id.SourceID); !os.IsNotExist(err) {
				t.Fatal("invalid backend URL reached filesystem creation")
			}
		})
	}
}

func openManagedRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func TestNativeSourceCreateAndReopen(t *testing.T) {
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	d, err := CreateNativeSource(managed, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Identity() != id {
		t.Fatalf("identity mismatch: %+v", d.Identity())
	}
	// Marker and directory modes.
	fi, err := os.Lstat(filepath.Join(dir, id.SourceID))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("source dir wrong: %+v %v", fi, err)
	}
	mi, err := os.Lstat(filepath.Join(dir, id.SourceID, nativeSourceOwnerFile))
	if err != nil || mi.Mode().Perm() != 0o600 {
		t.Fatalf("marker wrong: %+v %v", mi, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	d2, err := OpenNativeSource(managed, id)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.Identity() != id {
		t.Fatal("reopen identity mismatch")
	}
}

func TestNativeSourceBadIdentity(t *testing.T) {
	managed, _ := openManagedRoot(t)
	bad := []NativeSourceIdentity{
		func() NativeSourceIdentity { i := testIdentity(testSourceID()); i.SourceID = ""; return i }(),
		func() NativeSourceIdentity { i := testIdentity(testSourceID()); i.WorkspaceID = "not-a-uuid"; return i }(),
		func() NativeSourceIdentity {
			i := testIdentity(testSourceID())
			i.RuntimeID = "00000000-0000-0000-0000-000000000000"
			return i
		}(),
		func() NativeSourceIdentity {
			i := testIdentity(testSourceID())
			i.EnrollmentID = strings.ToUpper(testSourceID())
			return i
		}(),
		func() NativeSourceIdentity { i := testIdentity(testSourceID()); i.DaemonID = "  "; return i }(),
		func() NativeSourceIdentity { i := testIdentity(testSourceID()); i.BackendURL = "ftp://x"; return i }(),
		func() NativeSourceIdentity {
			i := testIdentity(testSourceID())
			i.BackendURL = "https://u:p@h"
			return i
		}(),
		func() NativeSourceIdentity {
			i := testIdentity(testSourceID())
			i.BackendURL = "https://h?q=1"
			return i
		}(),
	}
	for i, id := range bad {
		if _, err := CreateNativeSource(managed, id); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
		if _, err := OpenNativeSource(managed, id); err == nil {
			t.Fatalf("case %d reopen: expected error", i)
		}
	}
}

func TestNativeSourceExistingDataPreserved(t *testing.T) {
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	// Create must fail if ANY directory already exists at the leaf, even a
	// valid-looking one, and must not delete it.
	if err := os.Mkdir(filepath.Join(dir, id.SourceID), 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(dir, id.SourceID, "user-data.txt")
	if err := os.WriteFile(userFile, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateNativeSource(managed, id); err == nil {
		t.Fatal("expected create failure on existing dir")
	}
	if b, err := os.ReadFile(userFile); err != nil || string(b) != "precious" {
		t.Fatalf("existing data lost: %q %v", b, err)
	}
	// The pre-existing wrong-scope directory must be untouched: mode and
	// contents unchanged.
	fi, err := os.Lstat(filepath.Join(dir, id.SourceID))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir modified: perm=%v err=%v", fi.Mode().Perm(), err)
	}
}

func TestNativeSourceSymlinkMarkerAndLockRejected(t *testing.T) {
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	outside := t.TempDir()
	leaf := filepath.Join(dir, id.SourceID)
	for name, target := range map[string]string{
		"marker inside":  filepath.Join(dir, "elsewhere-marker"),
		"marker outside": filepath.Join(outside, "marker"),
		"lock inside":    filepath.Join(dir, "elsewhere-lock"),
		"lock outside":   filepath.Join(outside, "lock"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(leaf); err != nil {
				t.Fatal(err)
			}
			d, err := CreateNativeSource(managed, id)
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
			victim := nativeSourceOwnerFile
			if strings.Contains(name, "lock") {
				victim = nativeSourceLockFile
			}
			if err := os.Remove(filepath.Join(leaf, victim)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(leaf, victim)); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenNativeSource(managed, id); err == nil {
				t.Fatalf("%s symlink accepted", name)
			}
		})
	}
}

func TestNativeSourceBusyThenCloseReopen(t *testing.T) {
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	d, err := CreateNativeSource(managed, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenNativeSource(managed, id)
	if !errors.Is(err, ErrNativeSourceBusy) {
		t.Fatalf("expected busy, got %v", err)
	}
	// Deleting the lock file while the first holder still holds the (now
	// unlinked) inode must NOT let a second process mint a fresh lock and
	// double-hold the domain.
	lockPath := filepath.Join(dir, id.SourceID, nativeSourceLockFile)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	_, err = OpenNativeSource(managed, id)
	if err == nil {
		t.Fatal("reopen recreated missing lock while first holder held it")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore a lock file (an operator could recreate it after the crash
	// that unlinked it), then normal close/reopen works again.
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d2, err := OpenNativeSource(managed, id)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	d2.Close()
	// And with the lock missing entirely (no holder), reopen must still
	// refuse rather than recreate it.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenNativeSource(managed, id); err == nil {
		t.Fatal("reopen recreated missing lock file")
	}
}

func TestNativeSourceSymlinkLeafRejected(t *testing.T) {
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	// A real domain elsewhere in the managed tree...
	real, err := CreateNativeSource(managed, testIdentity("bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"))
	if err != nil {
		t.Fatal(err)
	}
	real.Close()
	// ...aliased under the SourceID name inside the managed root.
	if err := os.Symlink(
		filepath.Join(dir, "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"),
		filepath.Join(dir, id.SourceID),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateNativeSource(managed, id); err == nil {
		t.Fatal("create accepted symlink alias inside managed root")
	}
	if _, err := OpenNativeSource(managed, id); err == nil {
		t.Fatal("reopen accepted symlink alias inside managed root")
	}
	// Alias pointing outside the managed root.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "cccccccc-cccc-cccc-cccc-cccccccccccc")); err != nil {
		t.Fatal(err)
	}
	outsideID := testIdentity("cccccccc-cccc-cccc-cccc-cccccccccccc")
	if _, err := CreateNativeSource(managed, outsideID); err == nil {
		t.Fatal("create accepted symlink outside managed root")
	}
	if _, err := OpenNativeSource(managed, outsideID); err == nil {
		t.Fatal("reopen accepted symlink outside managed root")
	}
}

func TestNativeSourceMarkerFailClosed(t *testing.T) {
	managed, dir := openManagedRoot(t)
	id := testIdentity(testSourceID())
	markerPath := func() string {
		return filepath.Join(dir, id.SourceID, nativeSourceOwnerFile)
	}
	valid := `{"version":1,"backend_url":"https://backend.example.com","workspace_id":"11111111-1111-1111-1111-111111111111","source_id":"` + id.SourceID + `","runtime_id":"22222222-2222-2222-2222-222222222222","daemon_id":"daemon-1","enrollment_id":"33333333-3333-3333-3333-333333333333"}`

	cases := map[string]string{
		"truncated":     valid[:len(valid)/2],
		"unknown-field": strings.Replace(valid, "{", `{"extra":1,`, 1),
		"missing":       strings.Replace(valid, `"daemon_id":"daemon-1",`, "", 1),
		"null":          strings.Replace(valid, `"daemon-1"`, `null`, 1),
		"trailing":      valid + " garbage",
		"wrong-version": strings.Replace(valid, `"version":1`, `"version":2`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(filepath.Dir(markerPath())); err != nil {
				t.Fatal(err)
			}
			d, err := CreateNativeSource(managed, id)
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
			if err := os.WriteFile(markerPath(), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenNativeSource(managed, id); err == nil {
				t.Fatalf("%s marker reopened", name)
			}
			// Fail closed must not have deleted anything.
			if _, statErr := os.Lstat(markerPath()); statErr != nil {
				t.Fatalf("%s marker deleted: %v", name, statErr)
			}
		})
	}
	// Wrong identity in a well-formed marker.
	t.Run("wrong-identity", func(t *testing.T) {
		if err := os.RemoveAll(filepath.Dir(markerPath())); err != nil {
			t.Fatal(err)
		}
		other := testIdentity(id.SourceID)
		other.EnrollmentID = "44444444-4444-4444-4444-444444444444"
		d, err := CreateNativeSource(managed, other)
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
		if _, err := OpenNativeSource(managed, id); err == nil {
			t.Fatal("reopen adopted different identity")
		}
	})
}

func TestNativeSourceConcurrentCreateOneWinner(t *testing.T) {
	managed, _ := openManagedRoot(t)
	id := testIdentity(testSourceID())
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wins := make(chan *NativeSourceDomain, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := CreateNativeSource(managed, id)
			errs[i] = err
			if d != nil {
				wins <- d
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	winners := 0
	for d := range wins {
		winners++
		d.Close()
	}
	var failed int
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if winners != 1 || failed != n-1 {
		t.Fatalf("winners=%d failed=%d: %v", winners, failed, errs)
	}
	// The winner's marker must be intact and reopenable.
	d, err := OpenNativeSource(managed, id)
	if err != nil {
		t.Fatalf("reopen after race: %v", err)
	}
	d.Close()
}
