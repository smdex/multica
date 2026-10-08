package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Fixtures: canonical UUIDs, opaque node id, strict UTC time.
const (
	ngrTaskUUID   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	ngrIncarnUUID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	ngrGraphRun   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	ngrRuntime    = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	ngrNodeID     = "node#opaque-9!"
)

var ngrDispatchedAt = time.Date(2026, 10, 7, 12, 0, 0, 123456789, time.UTC)

// ngrEnv provisions a fresh held domain under an isolated HOME and returns the
// held domain plus its real manifest hash.
func ngrEnv(t *testing.T) (*execenv.NativeSourceDomain, string) {
	return ngrEnvVariant(t, nil)
}

// ngrEnvVariant provisions a held domain, optionally mutating the enrollment
// fixture before provisioning (used to build a genuinely different identity).
func ngrEnvVariant(t *testing.T, mutate func(*NativeSourceEnrollment)) (*execenv.NativeSourceDomain, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(cli.TaskConfigRootEnv, "")
	source := nseLocalSource()
	if mutate != nil {
		mutate(&source)
	}
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", source, ""); err != nil {
		t.Fatalf("provision: %v", err)
	}
	local, err := LoadNativeSourceEnrollmentLocal("", "http://127.0.0.1:1", source.WorkspaceID, source.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	root, err := os.OpenRoot(nseHomeNativeSourcesMust(t))
	if err != nil {
		t.Fatalf("open sources root: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	domain, err := execenv.OpenNativeSource(root, local.Identity)
	if err != nil {
		t.Fatalf("open held domain: %v", err)
	}
	t.Cleanup(func() { domain.Close() })
	return domain, local.ManifestHash
}

func nseHomeNativeSourcesMust(t *testing.T) string {
	t.Helper()
	base, err := nseHomeNativeSources()
	if err != nil {
		t.Fatalf("resolve native-sources root: %v", err)
	}
	return base
}

// ngrID builds the canonical reservation ID for the given domain.
func ngrID(domain *execenv.NativeSourceDomain, hash string) NativeGraphReservationID {
	return NativeGraphReservationID{
		GraphRunID:    ngrGraphRun,
		NativeNodeID:  ngrNodeID,
		Identity:      domain.Identity(),
		ManifestHash:  hash,
		TaskID:        ngrTaskUUID,
		RuntimeID:     ngrRuntime,
		DispatchedAt:  ngrDispatchedAt,
		IncarnationID: ngrIncarnUUID,
	}
}

func ngrAttemptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(nseHomeNativeSourcesMust(t), nseSourceID, nativeAttemptsDir, ngrTaskUUID+"."+ngrIncarnUUID)
}

// ngrOpen opens a reservation handle.
func ngrOpen(t *testing.T, domain *execenv.NativeSourceDomain, hash string) *NativeGraphReservation {
	t.Helper()
	r, err := OpenNativeGraphReservation(domain, ngrID(domain, hash))
	if err != nil {
		t.Fatalf("open reservation: %v", err)
	}
	return r
}

func TestNativeGraphReservationFullLifecycle(t *testing.T) {
	domain, hash := ngrEnv(t)
	r := ngrOpen(t, domain, hash)

	// Missing is not stopped.
	if st, err := r.Status(); err != nil || st.Reserved || st.Launching || st.Stopped {
		t.Fatalf("empty status: %+v %v", st, err)
	}
	if _, err := r.Stopped(); err != ErrNativeNotStopped {
		t.Fatalf("missing journal must be ErrNativeNotStopped, got %v", err)
	}

	if err := r.Reserve(); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Exact reserve replay is idempotent.
	if err := r.Reserve(); err != nil {
		t.Fatalf("reserve replay: %v", err)
	}
	// Reserved alone is not stopped.
	if _, err := r.Stopped(); err != ErrNativeNotStopped {
		t.Fatalf("reserved-only must not be stopped, got %v", err)
	}

	// Stop before launching rejects.
	if err := r.Stop(NativeGraphStoppedNamespaceWait); err == nil {
		t.Fatal("stop without launching must reject")
	}

	if created, err := r.BeginLaunch(); err != nil || !created {
		t.Fatalf("begin launch: created=%v %v", created, err)
	}
	// Launching replay after the winner: typed AlreadyLaunching, not nil.
	if _, err := r.BeginLaunch(); !errors.Is(err, ErrNativeAlreadyLaunching) {
		t.Fatalf("second begin launch must be ErrNativeAlreadyLaunching, got %v", err)
	}
	st, err := r.Status()
	if err != nil || !st.Reserved || !st.Launching || st.Stopped {
		t.Fatalf("launching status: %+v %v", st, err)
	}
	if _, err := r.Stopped(); err != ErrNativeNotStopped {
		t.Fatalf("launching must not be stopped, got %v", err)
	}

	if err := r.Stop(NativeGraphStoppedNamespaceWait); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Exact stopped replay idempotent.
	if err := r.Stop(NativeGraphStoppedNamespaceWait); err != nil {
		t.Fatalf("stop replay: %v", err)
	}
	// Mismatched stopped replay rejects.
	if err := r.Stop(NativeGraphStoppedSynchronousNoExec); err == nil {
		t.Fatal("changed stop kind replay must reject")
	}
	kind, err := r.Stopped()
	if err != nil || kind != NativeGraphStoppedNamespaceWait {
		t.Fatalf("stopped: %q %v", kind, err)
	}

	// Files exist at the canonical journal path with owner-only perms.
	dir := ngrAttemptPath(t)
	for _, name := range []string{nativeReservedFile, nativeLaunchingFile, nativeStoppedFile} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s perms %o", name, info.Mode().Perm())
		}
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("attempt dir perms: %v %v", info, err)
	}
}

func TestNativeGraphReservationChangedPayloadRejects(t *testing.T) {
	domain, hash := ngrEnv(t)
	r := ngrOpen(t, domain, hash)
	if err := r.Reserve(); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Same directory, changed generation (dispatched_at): open succeeds but
	// touching the existing record must reject the changed payload.
	changed, err := OpenNativeGraphReservation(domain, func() NativeGraphReservationID {
		id := ngrID(domain, hash)
		id.DispatchedAt = ngrDispatchedAt.Add(time.Second)
		return id
	}())
	if err != nil {
		t.Fatalf("open changed generation: %v", err)
	}
	if err := changed.Reserve(); err == nil {
		t.Fatal("changed generation payload must reject against existing record")
	}
	// Same directory, changed enrollment identity: open rejects because the
	// identity does not match the held domain.
	domain2, hash2 := ngrEnvVariant(t, func(s *NativeSourceEnrollment) {
		s.WorkspaceID = "33333333-3333-4333-8333-333333333333"
	})
	_ = hash2
	foreign := ngrID(domain2, hash2)
	// Point foreign ID at the first domain's journal via the first handle's
	// path components but with the second domain identity: open must reject.
	foreign.TaskID = ngrTaskUUID
	foreign.IncarnationID = ngrIncarnUUID
	if _, err := OpenNativeGraphReservation(domain, foreign); err == nil {
		t.Fatal("identity mismatch must reject at open")
	}
	// Non-canonical UUIDs and blank node id reject.
	bad := ngrID(domain, hash)
	bad.TaskID = strings.ToUpper(ngrTaskUUID)
	if _, err := OpenNativeGraphReservation(domain, bad); err == nil {
		t.Fatal("non-canonical task id must reject")
	}
	bad = ngrID(domain, hash)
	bad.NativeNodeID = "   "
	if _, err := OpenNativeGraphReservation(domain, bad); err == nil {
		t.Fatal("blank node id must reject")
	}
	bad = ngrID(domain, hash)
	bad.ManifestHash = ""
	if _, err := OpenNativeGraphReservation(domain, bad); err == nil {
		t.Fatal("missing manifest hash must reject")
	}
	// Wrong manifest hash vs held domain rejects.
	bad = ngrID(domain, hash)
	bad.ManifestHash = "deadbeef"
	if _, err := OpenNativeGraphReservation(domain, bad); err == nil {
		t.Fatal("mismatched manifest hash must reject")
	}
	// Non-UTC dispatched_at rejects.
	bad = ngrID(domain, hash)
	bad.DispatchedAt = ngrDispatchedAt.In(time.FixedZone("X", 3600))
	if _, err := OpenNativeGraphReservation(domain, bad); err == nil {
		t.Fatal("non-UTC dispatched_at must reject")
	}
}

func TestNativeGraphReservationCrashStageReopen(t *testing.T) {
	for _, stage := range []string{"reserved", "launching"} {
		t.Run(stage, func(t *testing.T) {
			domain, hash := ngrEnv(t)
			r := ngrOpen(t, domain, hash)
			if err := r.Reserve(); err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if stage == "launching" {
				if created, err := r.BeginLaunch(); err != nil || !created {
					t.Fatalf("begin launch: created=%v %v", created, err)
				}
			}
			// Crash: release the domain and reopen a fresh held domain (new
			// lock acquisition, simulating daemon restart). Pinned root
			// persists.
			if err := domain.Close(); err != nil {
				t.Fatalf("close domain: %v", err)
			}
			domain2, hash2 := reopenDomain(t)
			if hash2 != hash {
				t.Fatal("manifest hash must survive restart")
			}
			r2 := ngrOpen(t, domain2, hash2)
			st, err := r2.Status()
			if err != nil {
				t.Fatalf("status after crash: %v", err)
			}
			if !st.Reserved || st.Stopped {
				t.Fatalf("stage %s status: %+v", stage, st)
			}
			if (stage == "launching") != st.Launching {
				t.Fatalf("stage %s launching flag: %+v", stage, st)
			}
			// Launching reopens unknown without timeout/PID resets: the existing
			// immutable record stays; a duplicate begin is typed already-launching
			// (only in the launching stage) and never grants launch permission.
			if stage == "launching" {
				if _, err := r2.BeginLaunch(); !errors.Is(err, ErrNativeAlreadyLaunching) {
					t.Fatalf("begin launch after crash must be typed already-launching, got %v", err)
				}
			}
			// Stop requires a launch this journal attests. In the launching stage
			// the record already exists, so the duplicate begin is the typed
			// already-launching loss; in the reserved stage the reopen wins the
			// CAS.
			if _, err := r2.BeginLaunch(); stage == "launching" && !errors.Is(err, ErrNativeAlreadyLaunching) {
				t.Fatalf("begin launch after crash: %v", err)
			} else if stage == "reserved" && err != nil {
				t.Fatalf("begin launch after crash: %v", err)
			}
			if err := r2.Stop(NativeGraphStoppedSynchronousNoExec); err != nil {
				t.Fatalf("stop after crash: %v", err)
			}
		})
	}
}

// reopenDomain re-acquires the held domain at the canonical source path.
func reopenDomain(t *testing.T) (*execenv.NativeSourceDomain, string) {
	t.Helper()
	local, err := LoadNativeSourceEnrollmentLocal("", "http://127.0.0.1:1", nseWorkspace, nseSourceID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	root, err := os.OpenRoot(nseHomeNativeSourcesMust(t))
	if err != nil {
		t.Fatalf("open sources root: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	domain, err := execenv.OpenNativeSource(root, local.Identity)
	if err != nil {
		t.Fatalf("reopen domain: %v", err)
	}
	t.Cleanup(func() { domain.Close() })
	return domain, local.ManifestHash
}

func TestNativeGraphReservationCorruptForeignAndPerms(t *testing.T) {
	dirFile := func(name string) string {
		return filepath.Join(ngrAttemptPath(t), name)
	}

	t.Run("corrupt reserved", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dirFile(nativeReservedFile), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("corrupt reserved must fail closed")
		}
		if _, err := r.Status(); err == nil {
			t.Fatal("corrupt journal status must fail")
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(dirFile(nativeReservedFile))
		if err := os.WriteFile(dirFile(nativeReservedFile), append(data, '{'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("trailing data must reject")
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		var rec map[string]any
		data, _ := os.ReadFile(dirFile(nativeReservedFile))
		_ = json.Unmarshal(data, &rec)
		rec["extra"] = 1
		out, _ := json.Marshal(rec)
		if err := os.WriteFile(dirFile(nativeReservedFile), out, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("unknown field must reject")
		}
	})

	t.Run("public file perms", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dirFile(nativeReservedFile), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("public record must reject")
		}
		if _, err := r.Status(); err == nil {
			t.Fatal("public record status must fail")
		}
	})

	t.Run("public attempt dir", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ngrAttemptPath(t), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("public attempt dir must reject")
		}
		if _, err := r.Status(); err == nil {
			t.Fatal("public attempt dir status must fail")
		}
	})

	t.Run("symlink attempt dir", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		dir := ngrAttemptPath(t)
		elsewhere := filepath.Join(nseHomeNativeSourcesMust(t), nseSourceID, "elsewhere")
		if err := os.Rename(dir, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("symlinked attempt dir must reject")
		}
		if _, err := r.Status(); err == nil {
			t.Fatal("symlinked attempt dir status must fail")
		}
	})

	t.Run("foreign binding", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dirFile(nativeReservedFile), []byte(`{"version":1,"state":"reserved","graph_run_id":"`+ngrGraphRun+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(); err == nil {
			t.Fatal("record missing binding fields must reject")
		}
	})

	t.Run("stopped without launching", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		// Plant a valid stopped record with the exact binding but no
		// launching record.
		rec := nativeGraphRecord{
			Version:                  nativeGraphRecordVersion,
			State:                    nativeGraphStateStopped,
			NativeGraphReservationID: ngrID(domain, hash),
			StopKind:                 NativeGraphStoppedNamespaceWait,
			StoppedAt:                time.Now().UTC().Truncate(time.Second),
		}
		data, _ := json.Marshal(&rec)
		if err := os.WriteFile(dirFile(nativeStoppedFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Status(); err == nil {
			t.Fatal("stopped without launching must fail closed")
		}
		if _, err := r.Stopped(); err == nil {
			t.Fatal("stopped without launching must not yield stop evidence")
		}
	})
}

func TestNativeGraphReservationClosedDomainRefused(t *testing.T) {
	domain, hash := ngrEnv(t)
	if err := domain.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenNativeGraphReservation(domain, ngrID(domain, hash)); err == nil {
		t.Fatal("closed-domain handle must refuse")
	}
}

func TestNativeGraphReservationHeldRootRenameStillPinned(t *testing.T) {
	domain, hash := ngrEnv(t)
	r := ngrOpen(t, domain, hash)
	if err := r.Reserve(); err != nil {
		t.Fatal(err)
	}
	// Replace the on-disk source directory by rename; the pinned os.Root still
	// points at the original inode, so Status must keep observing the original
	// journal (or fail), never a different directory.
	base := nseHomeNativeSourcesMust(t)
	orig := filepath.Join(base, nseSourceID)
	if err := os.Rename(orig, filepath.Join(base, "moved-"+nseSourceID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(orig, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := r.Status()
	if err != nil {
		t.Fatalf("pinned status after rename failed: %v", err)
	}
	if !st.Reserved || st.Launching || st.Stopped {
		t.Fatalf("pinned status after rename: %+v", st)
	}
	// Publishing through the pinned handle still lands in the moved (pinned)
	// directory, not the decoy.
	if created, err := r.BeginLaunch(); err != nil || !created {
		t.Fatalf("begin launch through pinned root: created=%v %v", created, err)
	}
	decoy := filepath.Join(orig, nativeAttemptsDir)
	if _, err := os.Stat(decoy); !os.IsNotExist(err) {
		t.Fatalf("decoy directory observed journal state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "moved-"+nseSourceID, nativeAttemptsDir, ngrTaskUUID+"."+ngrIncarnUUID, nativeLaunchingFile)); err != nil {
		t.Fatalf("pinned launching.json missing: %v", err)
	}
}

func TestNativeGraphReservationBeginLaunchConcurrentSingleWinner(t *testing.T) {
	domain, hash := ngrEnv(t)
	r := ngrOpen(t, domain, hash)
	if err := r.Reserve(); err != nil {
		t.Fatal(err)
	}
	// Real link(2) CAS: exactly one of N concurrent BeginLaunch callers may
	// create the immutable launching record and thereby earn launch
	// permission. Everyone else gets ErrNativeAlreadyLaunching - never nil.
	const n = 8
	start := make(chan struct{})
	results := make([]struct {
		created bool
		err     error
	}, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			created, err := r.BeginLaunch()
			results[i].created = created
			results[i].err = err
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, res := range results {
		if res.err != nil {
			if !errors.Is(res.err, ErrNativeAlreadyLaunching) {
				t.Fatalf("loser must get ErrNativeAlreadyLaunching, got %v", res.err)
			}
			if res.created {
				t.Fatal("loser must not report created")
			}
			continue
		}
		if !res.created {
			t.Fatal("winner must report created=true")
		}
		winners++
	}
	if winners != 1 {
		t.Fatalf("exactly one BeginLaunch winner required, got %d", winners)
	}
	// A duplicate whose launch callback later fails is NOT proof the prior
	// attempt never launched: the shared attempt stays unknown. Calling Stop
	// from the non-owning duplicate would fabricate no_exec evidence; here we
	// only assert the journal remains launching-only (unknown), which is what
	// the root must conclude for that duplicate.
	st, err := r.Status()
	if err != nil || !st.Launching || st.Stopped {
		t.Fatalf("journal must remain launching/unknown after duplicate loss: %+v %v", st, err)
	}
}

// writeNativeGraphRecord writes a raw record JSON (state-metadata tampering).
func writeNativeGraphRecord(t *testing.T, name string, rec nativeGraphRecord) {
	t.Helper()
	data, err := json.Marshal(&rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ngrAttemptPath(t), name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGraphReservationBeginLaunchRequiresReserved(t *testing.T) {
	domain, hash := ngrEnv(t)
	r := ngrOpen(t, domain, hash)
	// No reserved record yet: BeginLaunch must fail closed.
	if _, err := r.BeginLaunch(); err == nil {
		t.Fatal("begin launch without reserved must reject")
	}
	if err := r.Reserve(); err != nil {
		t.Fatal(err)
	}
	if created, err := r.BeginLaunch(); err != nil || !created {
		t.Fatalf("begin launch after reserve: created=%v %v", created, err)
	}
	if err := r.Stop(NativeGraphStoppedNamespaceWait); err != nil {
		t.Fatal(err)
	}
	// Existing launching forbids a new Execute even after stop replay: a new
	// BeginLaunch after stopped evidence must reject.
	if _, err := r.BeginLaunch(); err == nil {
		t.Fatal("begin launch after stopped must reject")
	}
}

func TestNativeGraphReservationStateMetadataValidation(t *testing.T) {
	base := func(mutate func(*nativeGraphRecord)) nativeGraphRecord {
		rec := nativeGraphRecord{
			Version:                  nativeGraphRecordVersion,
			State:                    nativeGraphStateReserved,
			NativeGraphReservationID: ngrDispatchedFixture(t),
		}
		mutate(&rec)
		return rec
	}
	cases := []struct {
		name string
		rec  nativeGraphRecord
	}{
		{
			name: "reserved with stop_kind",
			rec:  base(func(r *nativeGraphRecord) { r.StopKind = NativeGraphStoppedNamespaceWait }),
		},
		{
			name: "reserved with stopped_at",
			rec:  base(func(r *nativeGraphRecord) { r.StoppedAt = time.Now().UTC().Truncate(time.Second) }),
		},
		{
			name: "stopped missing stop_kind",
			rec: base(func(r *nativeGraphRecord) {
				r.State = nativeGraphStateStopped
				r.StoppedAt = time.Now().UTC().Truncate(time.Second)
			}),
		},
		{
			name: "stopped invalid kind",
			rec: base(func(r *nativeGraphRecord) {
				r.State = nativeGraphStateStopped
				r.StopKind = "heartbeat_loss"
				r.StoppedAt = time.Now().UTC().Truncate(time.Second)
			}),
		},
		{
			name: "stopped zero stopped_at",
			rec: base(func(r *nativeGraphRecord) {
				r.State = nativeGraphStateStopped
				r.StopKind = NativeGraphStoppedNamespaceWait
			}),
		},
		{
			name: "stopped non-UTC stopped_at",
			rec: base(func(r *nativeGraphRecord) {
				r.State = nativeGraphStateStopped
				r.StopKind = NativeGraphStoppedNamespaceWait
				r.StoppedAt = time.Now().Truncate(time.Second).In(time.FixedZone("X", 3600))
			}),
		},
		{
			name: "unknown state",
			rec:  base(func(r *nativeGraphRecord) { r.State = "paused" }),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			domain, hash := ngrEnv(t)
			r := ngrOpen(t, domain, hash)
			if err := r.Reserve(); err != nil {
				t.Fatal(err)
			}
			writeNativeGraphRecord(t, nativeReservedFile, tc.rec)
			// Both transitions and reads must fail closed on the malformed
			// metadata.
			if _, err := r.BeginLaunch(); err == nil {
				t.Fatal("begin launch must reject malformed metadata")
			}
			if _, err := r.Status(); err == nil {
				t.Fatal("status must reject malformed metadata")
			}
			_ = hash
		})
	}
}

// ngrDispatchedFixture builds a valid ID without a live domain for metadata
// tampering tests; identity fields use the canonical nse fixture values.
func ngrDispatchedFixture(t *testing.T) NativeGraphReservationID {
	t.Helper()
	domain, hash := ngrEnv(t)
	id := ngrID(domain, hash)
	domain.Close()
	return id
}

func TestNativeGraphReservationWrongFilenameState(t *testing.T) {
	t.Run("launching file with reserved state", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		// Plant a record whose FILENAME says launching but state says reserved.
		reservedBytes, err := os.ReadFile(filepath.Join(ngrAttemptPath(t), nativeReservedFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ngrAttemptPath(t), nativeLaunchingFile), reservedBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.BeginLaunch(); err == nil {
			t.Fatal("launching.json carrying reserved state must reject a new begin")
		}
		if err := r.Stop(NativeGraphStoppedNamespaceWait); err == nil {
			t.Fatal("stop must reject launching.json with wrong state")
		}
		if _, err := r.Status(); err == nil {
			t.Fatal("status must reject wrong filename/state pairing")
		}
	})

	t.Run("stopped file with launching state", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		if created, err := r.BeginLaunch(); err != nil || !created {
			t.Fatalf("begin: %v", created)
		}
		launchingBytes, err := os.ReadFile(filepath.Join(ngrAttemptPath(t), nativeLaunchingFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ngrAttemptPath(t), nativeStoppedFile), launchingBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.Stop(NativeGraphStoppedNamespaceWait); err == nil {
			t.Fatal("stopped.json carrying launching state must reject stop replay")
		}
		if _, err := r.Stopped(); err == nil {
			t.Fatal("stopped evidence must reject wrong state in stopped.json")
		}
	})

	t.Run("reserved file with launching state", func(t *testing.T) {
		domain, hash := ngrEnv(t)
		r := ngrOpen(t, domain, hash)
		// Ensure the journal directory exists before planting the record.
		if err := r.Reserve(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(ngrAttemptPath(t), nativeReservedFile)); err != nil {
			t.Fatal(err)
		}
		// Plant launching-state record under reserved.json WITHOUT a valid
		// reserved record.
		rec := nativeGraphRecord{
			Version:                  nativeGraphRecordVersion,
			State:                    nativeGraphStateLaunching,
			NativeGraphReservationID: ngrID(domain, hash),
		}
		data, _ := json.Marshal(&rec)
		if err := os.WriteFile(filepath.Join(ngrAttemptPath(t), nativeReservedFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.BeginLaunch(); err == nil {
			t.Fatal("begin launch must reject reserved.json with launching state")
		}
	})
}

func TestNativeGraphReservationStopRequiresReservedChain(t *testing.T) {
	domain, hash := ngrEnv(t)
	r := ngrOpen(t, domain, hash)
	// launching.json exists but reserved.json is missing: stop must reject.
	if err := r.Reserve(); err != nil {
		t.Fatal(err)
	}
	if created, err := r.BeginLaunch(); err != nil || !created {
		t.Fatalf("begin: %v", err)
	}
	if err := os.Remove(filepath.Join(ngrAttemptPath(t), nativeReservedFile)); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(NativeGraphStoppedNamespaceWait); err == nil {
		t.Fatal("stop without reserved must reject")
	}
}
