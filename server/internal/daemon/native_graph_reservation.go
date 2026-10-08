package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Native graph launch reservation journal.
//
// Minimal append-only durable evidence beneath a HELD
// execenv.NativeSourceDomain.Root (the pinned physical source directory):
//
//	.native_attempts/<taskUUID>.<incarnationUUID>/
//	    reserved.json   - published before the server StartTask CAS
//	    launching.json  - atomically published before cmd.Start
//	    stopped.json    - positive qualified stop evidence
//
// Files bind graphRunID, nativeNodeID, the complete source identity, task /
// runtime / dispatched_at / incarnation and marker H (ManifestHash). State
// transitions are create-only (link(2)); nothing is ever rewritten or deleted.
// Every directory creation fsyncs the parent through the pinned root before
// publication. Missing is never stopped; reserved alone never proves
// never-launched without the caller's serialize-with-owner guarantee; no
// timeout or PID absence resets a launching reservation.

const (
	// nativeAttemptsDir is the journal root inside the held source domain.
	nativeAttemptsDir = ".native_attempts"
	// nativeAttemptDirMode mirrors the owner-only convention of the domain.
	nativeAttemptDirMode fs.FileMode = 0o700

	nativeReservedFile  = "reserved.json"
	nativeLaunchingFile = "launching.json"
	nativeStoppedFile   = "stopped.json"

	// nativeNodeIDMax bounds the opaque graph node identifier.
	nativeNodeIDMax = 256
	// nativeOwnerMarkerMax bounds the H marker string.
	nativeOwnerMarkerMax = 128
)

// ErrNativeNotStopped reports that no positive stop evidence exists for a
// reservation. A missing journal, or one holding only reserved/launching
// records, returns this error: absence of evidence is not stop proof.
var ErrNativeNotStopped = errors.New("native reservation: no positive stop evidence")

// NativeGraphReservationID is the validated identity a journal entry binds.
// All UUID fields are canonical non-zero; NativeNodeID is opaque, non-blank
// and bounded.
type NativeGraphReservationID struct {
	GraphRunID    string                       `json:"graph_run_id"`
	NativeNodeID  string                       `json:"native_node_id"`
	Identity      execenv.NativeSourceIdentity `json:"identity"`
	ManifestHash  string                       `json:"manifest_hash"`
	TaskID        string                       `json:"task_id"`
	RuntimeID     string                       `json:"runtime_id"`
	DispatchedAt  time.Time                    `json:"dispatched_at"`
	IncarnationID string                       `json:"incarnation_id"`
}

// validate checks the ID at the trust boundary.
func (id NativeGraphReservationID) validate() error {
	uuidFields := map[string]string{
		"graph_run_id":   id.GraphRunID,
		"task_id":        id.TaskID,
		"incarnation_id": id.IncarnationID,
	}
	for name, v := range uuidFields {
		if !nativeGraphCanonicalUUID(v) {
			return fmt.Errorf("native reservation: %s must be a canonical non-zero UUID, got %q", name, v)
		}
	}
	// RuntimeID participates in the domain identity (SourceID-qualified), not
	// the file path; canonical form is still required for exact binding.
	if !nativeGraphCanonicalUUID(id.RuntimeID) {
		return fmt.Errorf("native reservation: runtime_id must be a canonical non-zero UUID")
	}
	if strings.TrimSpace(id.NativeNodeID) == "" || len(id.NativeNodeID) > nativeNodeIDMax {
		return fmt.Errorf("native reservation: native_node_id must be non-blank and bounded")
	}
	if strings.TrimSpace(id.ManifestHash) == "" || len(id.ManifestHash) > nativeOwnerMarkerMax {
		return fmt.Errorf("native reservation: manifest hash must be non-blank and bounded")
	}
	if id.DispatchedAt.IsZero() || !validNativeGraphTime(id.DispatchedAt) {
		return errors.New("native reservation: dispatched_at must be a canonical UTC RFC3339 nanosecond time")
	}
	return nil
}

// matches reports exact binding equality across every field. Identity
// (struct) and time (exact instant) compare directly; canonical validation
// upstream rules out spelling variants of the UUID strings.
func (id NativeGraphReservationID) matches(other NativeGraphReservationID) bool {
	return id == other
}

// nativeGraphCanonicalUUID reuses the execenv canonical-UUID rule. It cannot
// call the unexported execenv helper, so it applies the same test locally.
func nativeGraphCanonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

// validNativeGraphTime requires the exact UTC RFC3339Nano spelling so that a
// rewritten time cannot alias an existing record through formatting variants.
func validNativeGraphTime(t time.Time) bool {
	if t.IsZero() || t.Location() != time.UTC {
		return false
	}
	// Round-trip through canonical RFC3339Nano: rejects any representation
	// that cannot be spelled canonically and compared exactly on disk.
	rt, err := time.Parse(time.RFC3339Nano, t.Format(time.RFC3339Nano))
	return err == nil && rt.Equal(t)
}

// nativeGraphState identifies one durable journal state.
type nativeGraphState string

const (
	nativeGraphStateReserved  nativeGraphState = "reserved"
	nativeGraphStateLaunching nativeGraphState = "launching"
	nativeGraphStateStopped   nativeGraphState = "stopped"
)

// NativeGraphStopKind is the only admitted stop evidence. Values are exact;
// unknown spellings are rejected on read.
type NativeGraphStopKind string

const (
	// NativeGraphStoppedNamespaceWait is positive evidence: the qualified
	// backend observed the namespace init's actual Wait return.
	NativeGraphStoppedNamespaceWait NativeGraphStopKind = "namespace_wait"
	// NativeGraphStoppedSynchronousNoExec is proof the provider executable
	// was never executed: a synchronous cmd.Start failure. An internal fork
	// before the exec is not excluded by this claim.
	NativeGraphStoppedSynchronousNoExec NativeGraphStopKind = "synchronous_no_exec"
)

// nativeGraphRecord is the strict on-disk shape shared by all three states.
// DisallowUnknownFields decoding rejects unknown fields; trailing data is
// rejected by readStrictRootFile.
type nativeGraphRecord struct {
	Version int              `json:"version"`
	State   nativeGraphState `json:"state"`
	NativeGraphReservationID
	// StopKind is set only on stopped records.
	StopKind NativeGraphStopKind `json:"stop_kind,omitempty"`
	// StoppedAt is set only on stopped records.
	StoppedAt time.Time `json:"stopped_at,omitempty"`
}

const nativeGraphRecordVersion = 1

// validateCommon checks the shared record invariants for state-independent
// fields.
func (r nativeGraphRecord) validateCommon() error {
	if r.Version != nativeGraphRecordVersion {
		return fmt.Errorf("native reservation: unknown record version %d", r.Version)
	}
	return r.NativeGraphReservationID.validate()
}

// validateState enforces the state-specific metadata shape: reserved and
// launching records carry NO stop metadata; stopped records must carry a
// valid stop kind and a canonical non-zero UTC StoppedAt.
func (r nativeGraphRecord) validateState() error {
	switch r.State {
	case nativeGraphStateReserved, nativeGraphStateLaunching:
		if r.StopKind != "" {
			return fmt.Errorf("native reservation: %s record carries stop_kind", r.State)
		}
		if !r.StoppedAt.IsZero() {
			return fmt.Errorf("native reservation: %s record carries stopped_at", r.State)
		}
	case nativeGraphStateStopped:
		if r.StopKind != NativeGraphStoppedNamespaceWait && r.StopKind != NativeGraphStoppedSynchronousNoExec {
			return fmt.Errorf("native reservation: stopped record has invalid stop kind %q", r.StopKind)
		}
		if !validNativeGraphTime(r.StoppedAt) {
			return errors.New("native reservation: stopped record has invalid canonical UTC stopped_at")
		}
	default:
		return fmt.Errorf("native reservation: unknown record state %q", r.State)
	}
	return nil
}

// NativeGraphReservation is a handle on one task/incarnation journal opened
// through a HELD source domain. Construct it via
// OpenNativeGraphReservation immediately after the domain is held and before
// any launch decision, so the pinned root cannot be swapped mid-flight.
type NativeGraphReservation struct {
	domain *execenv.NativeSourceDomain
	dir    string // canonical "<taskUUID>.<incarnationUUID>", validated
	id     NativeGraphReservationID
}

// OpenNativeGraphReservation validates id against the held domain's exact
// current identity and marker hash, then returns the journal handle. The
// domain must be held (non-nil, unlocked-by-nobody-else) by the caller; this
// function performs no locking itself.
func OpenNativeGraphReservation(domain *execenv.NativeSourceDomain, id NativeGraphReservationID) (*NativeGraphReservation, error) {
	if domain == nil || domain.Root == nil {
		return nil, errors.New("native reservation: a held source domain is required")
	}
	if err := id.validate(); err != nil {
		return nil, err
	}
	// Exact held identity: the caller's identity must equal the domain's
	// validated owner identity byte for byte.
	if domain.Identity() != id.Identity {
		return nil, errors.New("native reservation: identity does not match the held source domain")
	}
	// Exact held ManifestHash (H binds the local owner marker only).
	hash, err := domain.ManifestHash()
	if err != nil {
		return nil, fmt.Errorf("native reservation: attesting held domain: %w", err)
	}
	if hash != id.ManifestHash {
		return nil, errors.New("native reservation: manifest hash does not match the held domain marker")
	}
	return &NativeGraphReservation{
		domain: domain,
		dir:    id.TaskID + "." + id.IncarnationID,
		id:     id,
	}, nil
}

// openJournalDir opens the pinned attempt directory with strict validation.
// Each level is created/opened via openStrictSubRoot, which fsyncs the
// PARENT directory entry after creation (the domain root after
// .native_attempts, and .native_attempts after the attempt directory), so
// every directory entry leading to a record is durable before publication.
func (r *NativeGraphReservation) openJournalDir() (*os.Root, error) {
	attemptsRoot, err := openStrictSubRoot(r.domain.Root, nativeAttemptsDir, nativeAttemptsDir)
	if err != nil {
		return nil, err
	}
	defer attemptsRoot.Close()
	return openStrictSubRoot(attemptsRoot, r.dir, "attempt dir")
}

// openStrictSubRoot opens (creating with 0700 when missing) one strict
// owner-only real directory beneath parent, pins it with os.Root, rechecks
// identity via SameFile, and fsyncs the PARENT (".") through its pinned root
// so the new directory entry is durable before anything is published inside.
// The parent "." fsync runs on every call, not only after creation: it is
// cheap and keeps the durability claim uniform.
func openStrictSubRoot(parent *os.Root, leaf, what string) (*os.Root, error) {
	if parent == nil {
		return nil, fmt.Errorf("native reservation: parent root is required for %s", what)
	}
	if leaf != filepath.Base(leaf) || leaf == "" || leaf == "." || leaf == ".." {
		return nil, fmt.Errorf("native reservation: invalid %s name %q", what, leaf)
	}
	if err := parent.Mkdir(leaf, nativeAttemptDirMode); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("native reservation: create %s: %w", what, err)
	}
	info, err := parent.Lstat(leaf)
	if err != nil {
		return nil, fmt.Errorf("native reservation: inspect %s: %w", what, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("native reservation: %s must be a real directory", what)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("native reservation: %s must be owner-only (0700)", what)
	}
	sub, err := parent.OpenRoot(leaf)
	if err != nil {
		return nil, fmt.Errorf("native reservation: open %s: %w", what, err)
	}
	if rootInfo, err := sub.Stat("."); err != nil || !os.SameFile(info, rootInfo) {
		sub.Close()
		return nil, fmt.Errorf("native reservation: %s changed while opening", what)
	}
	// Parent-directory Sync: persists THIS directory's entry in parent. This
	// is the entry-creation durability; the sub directory's own contents are
	// synced separately after each record publication.
	if err := syncSelf(parent); err != nil {
		sub.Close()
		return nil, fmt.Errorf("native reservation: sync parent after %s: %w", what, err)
	}
	return sub, nil
}

// syncSelf opens "." through the pinned root and fsyncs that directory, so
// entry changes made through this root are flushed.
func syncSelf(root *os.Root) error {
	f, err := root.OpenFile(".", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// readRecord reads and validates one record. Missing is ErrNotExist.
func (r *NativeGraphReservation) readRecord(dirRoot *os.Root, name string) (*nativeGraphRecord, error) {
	var rec nativeGraphRecord
	if err := readStrictRootFile(dirRoot, name, &rec); err != nil {
		return nil, err
	}
	if err := rec.validateCommon(); err != nil {
		return nil, err
	}
	if err := rec.validateState(); err != nil {
		return nil, err
	}
	if !rec.matches(r.id) {
		return nil, fmt.Errorf("native reservation: %s binds a different reservation identity", name)
	}
	return &rec, nil
}

// matches compares the record's binding ID with the handle's exact ID.
func (r nativeGraphRecord) matches(id NativeGraphReservationID) bool {
	return r.NativeGraphReservationID.matches(id)
}

// requireTransition validates the journal preconditions for BeginLaunch: an
// exact reserved record exists and no stopped record does. A stopped attempt
// forbids a new launch of the same reservation identity.
func (r *NativeGraphReservation) requireTransition(dirRoot *os.Root) error {
	reserved, err := r.readRecord(dirRoot, nativeReservedFile)
	if err != nil {
		return fmt.Errorf("native reservation: begin launch requires a matching reserved record: %w", err)
	}
	if reserved.State != nativeGraphStateReserved {
		return fmt.Errorf("native reservation: %s has state %q", nativeReservedFile, reserved.State)
	}
	if stopped, err := r.readRecord(dirRoot, nativeStoppedFile); err == nil && stopped.State == nativeGraphStateStopped {
		return errors.New("native reservation: begin launch forbidden after stopped evidence")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Reserve publishes reserved.json. The exact record replays idempotently; any
// changed payload rejects. The caller must separately guarantee that a replay
// of an existing reserved-only record serializes with the local owner before
// treating the reservation as never_launched.
func (r *NativeGraphReservation) Reserve() error {
	return r.publishState(nativeReservedFile, nativeGraphRecord{
		Version:                  nativeGraphRecordVersion,
		State:                    nativeGraphStateReserved,
		NativeGraphReservationID: r.id,
	})
}

// ErrNativeAlreadyLaunching reports that launching.json already exists, so
// THIS invocation did not create it and must not execute a launch. Exactly
// one concurrent BeginLaunch winner may proceed to cmd.Start; every other
// invocation gets this typed error. It is NOT permission to launch and NOT
// evidence about the prior attempt: a callback failure received while this
// error is pending leaves the shared attempt's state unknown - the duplicate
// invocation never executed, so it can neither claim the launch nor record
// stopped/synchronous_no_exec for an attempt it did not own.
var ErrNativeAlreadyLaunching = errors.New("native reservation: launching record already exists")

// BeginLaunch atomically publishes launching.json and reports whether THIS
// invocation created the immutable record. The create-only link(2) inside
// writeStrictRootFile is the CAS: among concurrent callers exactly one gets
// created=true, the others get ErrNativeAlreadyLaunching (after the existing
// record is validated against this handle's exact binding; a changed payload
// rejects). No timeout or PID reset applies - an existing launching record
// stays forever until positive stop evidence replaces uncertainty.
func (r *NativeGraphReservation) BeginLaunch() (created bool, err error) {
	dirRoot, err := r.openJournalDir()
	if err != nil {
		return false, err
	}
	defer dirRoot.Close()
	// Transition gate: launching requires an exact reserved record and must
	// not follow an already-stopped attempt.
	if err := r.requireTransition(dirRoot); err != nil {
		return false, err
	}
	err = writeStrictRootFile(dirRoot, nativeLaunchingFile, &nativeGraphRecord{
		Version:                  nativeGraphRecordVersion,
		State:                    nativeGraphStateLaunching,
		NativeGraphReservationID: r.id,
	})
	if err == nil {
		// The record link is durable only after the containing directory's
		// entry change is synced; the ack/launch permission waits for it.
		if serr := syncSelf(dirRoot); serr != nil {
			return false, fmt.Errorf("native reservation: sync dir after %s: %w", nativeLaunchingFile, serr)
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("native reservation: publish %s: %w", nativeLaunchingFile, err)
	}
	// Link CAS lost: the record already exists. Validate it binds exactly
	// this reservation, then report the typed loss - never nil, which a
	// caller could mistake for launch permission.
	existing, rerr := r.readRecord(dirRoot, nativeLaunchingFile)
	if rerr != nil {
		return false, fmt.Errorf("native reservation: validate existing %s: %w", nativeLaunchingFile, rerr)
	}
	if existing.State != nativeGraphStateLaunching {
		return false, fmt.Errorf("native reservation: %s has state %q", nativeLaunchingFile, existing.State)
	}
	return false, ErrNativeAlreadyLaunching
}

// Stop publishes stopped.json. Requires launching.json to exist with the
// exact binding. kind must be namespace_wait (the qualified backend's
// positive namespace-init Wait evidence) or synchronous_no_exec (a
// synchronous cmd.Start failure: the provider executable was never executed;
// an internal fork before exec is not excluded); a semantic
// result, stdout EOF or heartbeat loss is never stop evidence. An existing
// identical stopped record replays; a mismatched one rejects.
func (r *NativeGraphReservation) Stop(kind NativeGraphStopKind) error {
	if kind != NativeGraphStoppedNamespaceWait && kind != NativeGraphStoppedSynchronousNoExec {
		return fmt.Errorf("native reservation: unknown stop kind %q", kind)
	}
	now := time.Now().UTC().Truncate(time.Second)
	dirRoot, err := r.openJournalDir()
	if err != nil {
		return err
	}
	defer dirRoot.Close()
	// Stop requires the full reservation chain: an exact reserved record and
	// an exact launching record whose file carries the launching state.
	reserved, err := r.readRecord(dirRoot, nativeReservedFile)
	if err != nil {
		return fmt.Errorf("native reservation: stop requires a matching reserved record: %w", err)
	}
	if reserved.State != nativeGraphStateReserved {
		return fmt.Errorf("native reservation: %s has state %q, expected %q", nativeReservedFile, reserved.State, nativeGraphStateReserved)
	}
	launching, err := r.readRecord(dirRoot, nativeLaunchingFile)
	if err != nil {
		return fmt.Errorf("native reservation: stop requires a matching launching record: %w", err)
	}
	if launching.State != nativeGraphStateLaunching {
		return fmt.Errorf("native reservation: %s has state %q, expected %q", nativeLaunchingFile, launching.State, nativeGraphStateLaunching)
	}
	existing, err := r.readRecord(dirRoot, nativeStoppedFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Corrupt or foreign stopped record: fail closed, never overwrite.
		return err
	}
	rec := nativeGraphRecord{
		Version:                  nativeGraphRecordVersion,
		State:                    nativeGraphStateStopped,
		NativeGraphReservationID: r.id,
		StopKind:                 kind,
		StoppedAt:                now,
	}
	if existing != nil {
		// Exact replay (same kind; binding already matched in readRecord) is
		// idempotent. Anything else rejects. The retry must sync BEFORE
		// acking: a prior attempt may have linked and failed its sync.
		if existing.StopKind != kind {
			return fmt.Errorf("native reservation: stopped record exists with different stop kind %q", existing.StopKind)
		}
		if existing.State != nativeGraphStateStopped {
			return fmt.Errorf("native reservation: %s has state %q, expected %q", nativeStoppedFile, existing.State, nativeGraphStateStopped)
		}
		if err := syncSelf(dirRoot); err != nil {
			return fmt.Errorf("native reservation: sync dir on %s replay: %w", nativeStoppedFile, err)
		}
		return nil
	}
	if err := writeStrictRootFile(dirRoot, nativeStoppedFile, &rec); err != nil {
		return fmt.Errorf("native reservation: publish %s: %w", nativeStoppedFile, err)
	}
	// Directory sync before any caller treats the attempt as stopped.
	if err := syncSelf(dirRoot); err != nil {
		return fmt.Errorf("native reservation: sync dir after %s: %w", nativeStoppedFile, err)
	}
	return nil
}

// publishState publishes reserved/launching records with exact-replay
// idempotence and changed-payload rejection.
func (r *NativeGraphReservation) publishState(name string, rec nativeGraphRecord) error {
	dirRoot, err := r.openJournalDir()
	if err != nil {
		return err
	}
	defer dirRoot.Close()
	existing, err := r.readRecord(dirRoot, name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A torn, corrupt, public-perm or foreign record fails closed. It is
		// never overwritten.
		return err
	}
	if existing != nil {
		if existing.State != rec.State {
			return fmt.Errorf("native reservation: %s has state %q, expected %q", name, existing.State, rec.State)
		}
		// Exact binding already matched in readRecord; reserved and
		// launching records carry no payload beyond the binding, so this is
		// an exact replay. A prior attempt may have linked successfully and
		// failed its directory sync, so the retry must sync BEFORE acking:
		// never acknowledge durability that was not confirmed.
		if err := syncSelf(dirRoot); err != nil {
			return fmt.Errorf("native reservation: sync dir on %s replay: %w", name, err)
		}
		return nil
	}
	if err := writeStrictRootFile(dirRoot, name, &rec); err != nil {
		return fmt.Errorf("native reservation: publish %s: %w", name, err)
	}
	// Directory sync before any caller treats the reservation as established.
	if err := syncSelf(dirRoot); err != nil {
		return fmt.Errorf("native reservation: sync dir after %s: %w", name, err)
	}
	return nil
}

// NativeGraphReservationStatus is the observed journal state.
type NativeGraphReservationStatus struct {
	// Reserved reports a valid reserved.json exists.
	Reserved bool
	// Launching reports a valid launching.json exists. Any process may be
	// alive; no timeout or PID resets this.
	Launching bool
	// Stopped reports valid positive stop evidence exists, with its kind.
	Stopped  bool
	StopKind NativeGraphStopKind
	// StoppedAt is the recorded stop time when Stopped.
	StoppedAt time.Time
}

// Status reads the journal through the pinned held root. A missing journal is
// reported as all-false (missing is not stopped); any corrupt, foreign or
// accessible-to-others record fails closed.
func (r *NativeGraphReservation) Status() (NativeGraphReservationStatus, error) {
	var out NativeGraphReservationStatus
	// A missing attempt directory is a clean empty journal.
	if info, err := r.domain.Root.Lstat(nativeAttemptsDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("native reservation: inspect %s: %w", nativeAttemptsDir, err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return out, fmt.Errorf("native reservation: %s must be an owner-only real directory", nativeAttemptsDir)
	}
	attemptsRoot, err := openStrictSubRoot(r.domain.Root, nativeAttemptsDir, nativeAttemptsDir)
	if err != nil {
		return out, err
	}
	defer attemptsRoot.Close()
	if info, err := attemptsRoot.Lstat(r.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("native reservation: inspect attempt dir: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return out, errors.New("native reservation: attempt dir must be an owner-only real directory")
	}
	dirRoot, err := openStrictSubRoot(attemptsRoot, r.dir, "attempt dir")
	if err != nil {
		return out, err
	}
	defer dirRoot.Close()

	read := func(name string) (*nativeGraphRecord, error) {
		rec, err := r.readRecord(dirRoot, name)
		if err != nil && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return rec, err
	}
	reserved, err := read(nativeReservedFile)
	if err != nil {
		return out, err
	}
	launching, err := read(nativeLaunchingFile)
	if err != nil {
		return out, err
	}
	stopped, err := read(nativeStoppedFile)
	if err != nil {
		return out, err
	}
	// launch-without-reserved and stop-without-launching are torn states: an
	// unknown-state record set fails closed rather than being interpreted.
	if reserved != nil && reserved.State != nativeGraphStateReserved {
		return out, errors.New("native reservation: reserved.json has wrong state")
	}
	if launching != nil && launching.State != nativeGraphStateLaunching {
		return out, errors.New("native reservation: launching.json has wrong state")
	}
	if launching != nil && reserved == nil {
		return out, errors.New("native reservation: launching without reserved")
	}
	if stopped != nil {
		if stopped.State != nativeGraphStateStopped {
			return out, errors.New("native reservation: stopped.json has wrong state")
		}
		if stopped.StopKind != NativeGraphStoppedNamespaceWait && stopped.StopKind != NativeGraphStoppedSynchronousNoExec {
			return out, fmt.Errorf("native reservation: stopped.json has unknown stop kind %q", stopped.StopKind)
		}
		if launching == nil {
			return out, errors.New("native reservation: stopped without launching")
		}
		out.Stopped = true
		out.StopKind = stopped.StopKind
		out.StoppedAt = stopped.StoppedAt
	}
	out.Reserved = reserved != nil
	out.Launching = launching != nil
	return out, nil
}

// Stopped returns valid positive stop evidence for the reservation. It
// returns ErrNativeNotStopped when no stopped.json exists; reserved-only or
// launching journals never count as stopped. Absence of a PID or a launched
// hint is never stop proof, so no such field is consulted.
func (r *NativeGraphReservation) Stopped() (NativeGraphStopKind, error) {
	st, err := r.Status()
	if err != nil {
		return "", err
	}
	if !st.Stopped {
		return "", ErrNativeNotStopped
	}
	return st.StopKind, nil
}
