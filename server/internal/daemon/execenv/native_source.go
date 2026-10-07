package execenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
)

// Native source enrollment state, isolated to a dedicated directory beneath a
// caller-pinned managed os.Root. See CreateNativeSource / OpenNativeSource.

const (
	// nativeSourceOwnerFile records WHO owns the directory. The lock file
	// separately answers whether that owner is still running. The same
	// split as envRootOwnerFile / envRootLockFile.
	nativeSourceOwnerFile = ".native_owner"
	// nativeSourceLockFile carries the exclusive OS lock held until Close.
	// The kernel releases it when the process dies, so a crashed daemon
	// never wedges the domain. The lock excludes cooperating Multica
	// daemons only. It is not a quiescence guarantee against arbitrary
	// external programs and says nothing about child process trees.
	nativeSourceLockFile = ".native_lock"

	nativeSourceMarkerVersion = 1
	// nativeSourceMarkerMax bounds marker reads so a swapped-in large file
	// cannot exhaust memory before validation rejects it.
	nativeSourceMarkerMax = 8 << 10
)

// NativeSourceIdentity names one native enrollment domain. WorkspaceID,
// SourceID, RuntimeID and EnrollmentID are canonical non-zero UUIDs (the
// EnrollmentID acts as an independent nonce; it is not required to relate to
// SourceID). DaemonID is an opaque non-blank string.
type NativeSourceIdentity struct {
	BackendURL   string `json:"backend_url"`
	WorkspaceID  string `json:"workspace_id"`
	SourceID     string `json:"source_id"`
	RuntimeID    string `json:"runtime_id"`
	DaemonID     string `json:"daemon_id"`
	EnrollmentID string `json:"enrollment_id"`
}

// nativeSourceMarker is the strict v1 on-disk shape: version plus the owner
// identity. Missing or null JSON fields decode to zero values, which the
// version check and identity validation reject, so presence is enforced
// without a second decode pass.
type nativeSourceMarker struct {
	Version int `json:"version"`
	NativeSourceIdentity
}

// ErrNativeSourceBusy reports that another live process holds the domain's
// lock.
var ErrNativeSourceBusy = errors.New("native source is held by a running process")

// NativeSourceDomain is a held native enrollment domain: an os.Root pinned to
// the physical source directory plus the exclusive lock. Close releases both.
type NativeSourceDomain struct {
	// Root is the pinned source directory. All reads and writes of source
	// data must go through it so nothing re-resolves the leaf by name after
	// the pin. Nil after Close.
	Root *os.Root
	// Path is the display path of the source directory leaf name.
	Path string

	lock *os.File
	id   NativeSourceIdentity
}

// Identity returns the validated owner identity of the held domain.
func (d *NativeSourceDomain) Identity() NativeSourceIdentity { return d.id }

// ManifestHash binds approval to the validated immutable marker, not a path.
func (d *NativeSourceDomain) ManifestHash() (string, error) {
	if d == nil || d.Root == nil || d.lock == nil {
		return "", errors.New("execenv: native source is not held")
	}
	marker, err := readNativeSourceMarker(d.Root)
	if err != nil || marker == nil || marker.NativeSourceIdentity != d.id {
		return "", errors.New("execenv: native source marker changed")
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// Close releases the lock and the pinned root. Safe on nil and idempotent.
// It never deletes on-disk data.
func (d *NativeSourceDomain) Close() error {
	if d == nil {
		return nil
	}
	var err error
	if d.lock != nil {
		_ = unlockFile(d.lock)
		err = d.lock.Close()
		d.lock = nil
	}
	if d.Root != nil {
		err = errors.Join(err, d.Root.Close())
		d.Root = nil
	}
	return err
}

// validateNativeSourceIdentity checks the identity at the trust boundary.
func validateNativeSourceIdentity(id NativeSourceIdentity) error {
	for name, v := range map[string]string{
		"workspace_id":  id.WorkspaceID,
		"source_id":     id.SourceID,
		"runtime_id":    id.RuntimeID,
		"enrollment_id": id.EnrollmentID,
	} {
		if !isCanonicalUUID(v) {
			return fmt.Errorf("execenv: native source %s must be a canonical non-zero UUID, got %q", name, v)
		}
	}
	if strings.TrimSpace(id.DaemonID) == "" || len(id.DaemonID) > 256 {
		return fmt.Errorf("execenv: native source daemon_id must be non-blank and bounded")
	}
	if err := validateNativeSourceBackendURL(id.BackendURL); err != nil {
		return err
	}
	return nil
}

// validateNativeSourceBackendURL requires a canonical http(s) URL without
// query, fragment or userinfo, bounded in length.
func validateNativeSourceBackendURL(raw string) error {
	if raw == "" || len(raw) > 2048 {
		return fmt.Errorf("execenv: native source backend_url must be non-blank and bounded")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != raw {
		return errors.New("execenv: native source backend_url must be canonical http(s) with a host and no credentials, query or fragment")
	}
	return nil
}

// isCanonicalUUID reports whether s is exactly the canonical lowercase hyphen
// form of a non-zero UUID. Requiring canonical form means the value is safe as
// a single path element and that string comparison of identities cannot be
// fooled by case or formatting variants.
func isCanonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

// CreateNativeSource creates and holds a fresh domain for id directly beneath
// the caller-pinned managed root. The leaf directory is named by the canonical
// SourceID and created with O_EXCL semantics: any pre-existing directory or
// symlink at that name, even a valid one, fails creation. Existing data is
// never adopted, replaced or deleted on any failure path.
func CreateNativeSource(managed *os.Root, id NativeSourceIdentity) (*NativeSourceDomain, error) {
	if managed == nil {
		return nil, errors.New("execenv: managed root is required")
	}
	if err := validateNativeSourceIdentity(id); err != nil {
		return nil, err
	}
	// sourceID is validated canonical, so it is a single safe path element.
	// No arbitrary existing path can be imported or observed here. Mkdir
	// already creates the leaf with 0700; an existing leaf is never
	// modified.
	// ponytail: a crash before marker publication requires explicit operator
	// cleanup. Automatic recovery would require a durable create journal.
	if err := managed.Mkdir(id.SourceID, 0o700); err != nil {
		return nil, fmt.Errorf("execenv: create native source %s: %w", id.SourceID, err)
	}
	d, err := openHeldNativeSource(managed, id, true)
	if err != nil {
		return nil, err
	}
	if err := writeNativeSourceMarker(d.Root, id); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// OpenNativeSource reopens an existing domain and strictly validates its
// marker against id. It never replaces, resets or adopts: any mismatch,
// truncation, unknown shape or symlink fails closed with the data untouched.
// Returns ErrNativeSourceBusy while another process holds the lock.
func OpenNativeSource(managed *os.Root, id NativeSourceIdentity) (*NativeSourceDomain, error) {
	if managed == nil {
		return nil, errors.New("execenv: managed root is required")
	}
	if err := validateNativeSourceIdentity(id); err != nil {
		return nil, err
	}
	return openHeldNativeSource(managed, id, false)
}

// openHeldNativeSource pins the leaf directory through the managed root,
// rejects symlink aliases, takes the exclusive lock, then strictly validates
// the marker against the pinned root. On any error every opened handle is
// closed; on-disk data is never deleted.
func openHeldNativeSource(managed *os.Root, id NativeSourceIdentity, expectFresh bool) (*NativeSourceDomain, error) {
	// Lstat through the managed root: a symlink leaf is rejected even when it
	// points somewhere inside the managed tree, so no alias can be followed.
	info, err := managed.Lstat(id.SourceID)
	if err != nil {
		return nil, fmt.Errorf("execenv: inspect native source %s: %w", id.SourceID, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("execenv: native source %s must be a private directory", id.SourceID)
	}
	// Pin the physical directory. Everything after this resolves through sub,
	// never by re-resolving the leaf name from managed.
	sub, err := managed.OpenRoot(id.SourceID)
	if err != nil {
		return nil, fmt.Errorf("execenv: pin native source %s: %w", id.SourceID, err)
	}
	var lock *os.File
	fail := func(err error) (*NativeSourceDomain, error) {
		if lock != nil {
			lock.Close()
		}
		sub.Close()
		return nil, err
	}
	subInfo, err := sub.Stat(".")
	if err != nil {
		return fail(fmt.Errorf("execenv: stat native source %s: %w", id.SourceID, err))
	}
	// Bind everything to the same physical subroot we pinned: if the leaf was
	// swapped between Lstat and the pin, the pinned identity differs from the
	// inspected one.
	if !os.SameFile(info, subInfo) {
		return fail(fmt.Errorf("execenv: native source %s changed while pinning", id.SourceID))
	}

	// Lock first, then validate the marker against the pinned root, so no
	// window exists where we act on marker state without holding the lock.
	// A pre-existing lock must be an existing regular file, opened without
	// O_CREATE: recreating a missing lock would let a second holder mint a
	// new inode while the old owner still holds the unlinked one. Only a
	// freshly created domain may create the lock, with O_EXCL.
	lockInfo, err := lstatNativeSourceLock(sub)
	if err != nil {
		return fail(err)
	}
	if expectFresh {
		if lockInfo != nil {
			return fail(errors.New("execenv: fresh native source already has a lock"))
		}
		lock, err = sub.OpenFile(nativeSourceLockFile, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	} else {
		if lockInfo == nil {
			return fail(errors.New("execenv: native source has no lock; refusing to recreate it"))
		}
		lock, err = sub.OpenFile(nativeSourceLockFile, os.O_RDWR, 0)
	}
	if err != nil {
		return fail(fmt.Errorf("execenv: open native source lock: %w", err))
	}
	openInfo, err := lock.Stat()
	if err != nil || !openInfo.Mode().IsRegular() {
		return fail(errors.New("execenv: native source lock is not a regular file"))
	}
	currentInfo, err := lstatNativeSourceLock(sub)
	if err != nil || currentInfo == nil || !os.SameFile(openInfo, currentInfo) || (lockInfo != nil && !os.SameFile(lockInfo, openInfo)) {
		return fail(errors.New("execenv: native source lock changed while opening"))
	}
	locked, err := lockFileExclusiveNonBlocking(lock)
	if err != nil {
		return fail(fmt.Errorf("execenv: lock native source %s: %w", id.SourceID, err))
	}
	if !locked {
		return fail(fmt.Errorf("execenv: open native source %s: %w", id.SourceID, ErrNativeSourceBusy))
	}

	currentInfo, err = lstatNativeSourceLock(sub)
	if err != nil || currentInfo == nil || !os.SameFile(openInfo, currentInfo) {
		return fail(errors.New("execenv: native source lock changed while acquiring ownership"))
	}
	marker, err := readNativeSourceMarker(sub)
	if err != nil {
		return fail(err)
	}
	if expectFresh {
		if marker != nil {
			return fail(fmt.Errorf("execenv: native source %s already has an owner marker", id.SourceID))
		}
	} else if marker == nil {
		return fail(fmt.Errorf("execenv: native source %s has no owner marker; refusing to adopt it", id.SourceID))
	} else if marker.NativeSourceIdentity != id {
		return fail(fmt.Errorf("execenv: native source %s belongs to a different identity; refusing to reset or adopt it", id.SourceID))
	}

	return &NativeSourceDomain{Root: sub, Path: sub.Name(), lock: lock, id: id}, nil
}

// lstatNativeSourceLock Lstats the lock name, rejecting a symlink or
// non-regular file. A missing file returns (nil, nil).
func lstatNativeSourceLock(sub *os.Root) (os.FileInfo, error) {
	info, err := sub.Lstat(nativeSourceLockFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("execenv: inspect native source lock: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("execenv: native source lock must be a private regular file")
	}
	return info, nil
}

// writeNativeSourceMarker publishes a create-only marker and syncs its contents.
// ponytail: parent-directory power-loss durability is not portable here. A lost
// or torn marker fails closed, and needs explicit cleanup, never adoption.
func writeNativeSourceMarker(sub *os.Root, id NativeSourceIdentity) error {
	data, err := json.Marshal(nativeSourceMarker{
		Version:              nativeSourceMarkerVersion,
		NativeSourceIdentity: id,
	})
	if err != nil {
		return fmt.Errorf("execenv: encode native source owner: %w", err)
	}
	f, err := sub.OpenFile(nativeSourceOwnerFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("execenv: create native source owner marker: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("execenv: write native source owner marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("execenv: sync native source owner marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("execenv: close native source owner marker: %w", err)
	}
	return nil
}

// readNativeSourceMarker reads and strictly validates the owner marker through
// the pinned root. It returns (nil, nil) only when the marker is absent. Any
// truncation, null, missing or unknown field, wrong version, wrong shape or
// trailing bytes fails closed. The marker must also be a regular non-symlink
// file whose opened handle matches the Lstat identity.
func readNativeSourceMarker(sub *os.Root) (*nativeSourceMarker, error) {
	lstat, err := sub.Lstat(nativeSourceOwnerFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("execenv: inspect native source owner marker: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 || !lstat.Mode().IsRegular() || lstat.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("execenv: native source owner marker must be a private regular file")
	}
	f, err := sub.OpenFile(nativeSourceOwnerFile, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("execenv: open native source owner marker: %w", err)
	}
	defer f.Close()
	if openStat, statErr := f.Stat(); statErr != nil {
		return nil, fmt.Errorf("execenv: stat native source owner marker: %w", statErr)
	} else if !os.SameFile(lstat, openStat) {
		return nil, fmt.Errorf("execenv: native source owner marker changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, nativeSourceMarkerMax+1))
	if err != nil {
		return nil, fmt.Errorf("execenv: read native source owner marker: %w", err)
	}
	if len(data) > nativeSourceMarkerMax {
		return nil, fmt.Errorf("execenv: native source owner marker exceeds %d bytes", nativeSourceMarkerMax)
	}
	m, err := decodeNativeSourceMarker(data)
	if err != nil {
		return nil, fmt.Errorf("execenv: decode native source owner marker: %w", err)
	}
	return &m, nil
}

// decodeNativeSourceMarker decodes one strict JSON object: no trailing bytes,
// no unknown fields, version exactly 1, and a valid identity. Missing or null
// fields decode to zero values that the version and identity checks reject.
func decodeNativeSourceMarker(data []byte) (nativeSourceMarker, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var m nativeSourceMarker
	if err := dec.Decode(&m); err != nil {
		return nativeSourceMarker{}, err
	}
	// Exactly one JSON value, no trailing bytes.
	if _, err := dec.Token(); err != io.EOF {
		return nativeSourceMarker{}, errors.New("trailing data after marker object")
	}
	if m.Version != nativeSourceMarkerVersion {
		return nativeSourceMarker{}, fmt.Errorf("unsupported marker version %d", m.Version)
	}
	if err := validateNativeSourceIdentity(m.NativeSourceIdentity); err != nil {
		return nativeSourceMarker{}, fmt.Errorf("marker identity: %w", err)
	}
	return m, nil
}
