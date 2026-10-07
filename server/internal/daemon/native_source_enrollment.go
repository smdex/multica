package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Native source enrollment: local provisioning, per-profile manifest index and
// approval staging, and the daemon enrollment reconciliation loop.
//
// Physical domains live in a MACHINE-GLOBAL tree - cli.ProfileDir("")/native-
// sources/<SourceID> - independent of the selected profile, because the
// SourceID leaf is the physical identity and two profiles must never mint two
// domains for one source. Profiles own only an immutable index of the sources
// provisioned through them and an ephemeral approval inbox.
//
// ponytail: a lost or torn profile index has no automatic repair. The domain
// marker stays authoritative; repairing an index means rerunning provision with
// the same identity while the domain is unlocked, or manual copy. No
// parent-directory power-loss durability is claimed anywhere here.

const (
	// nativeSourcesDir is the leaf below the machine-global profile root.
	nativeSourcesDir = "native-sources"
	// nativeSourceIndexDir is the per-profile immutable index directory.
	nativeSourceIndexDir = "native-source-enrollments"
	// nativeSourceInboxDir is the per-profile ephemeral approval inbox.
	nativeSourceInboxDir = "inbox"

	// nativeSourceFileMax bounds every JSON object read here (index, packet).
	nativeSourceFileMax = 16 << 10

	nativeEnrollmentLoopPoll = time.Second
	// nativeEnrollmentRetryPause separates transient finalize retries.
	nativeEnrollmentRetryPause = 250 * time.Millisecond
)

// NativeSourceEnrollmentLocal is what LoadNativeSourceEnrollmentLocal returns:
// the immutable owner identity plus the manifest hash derived from it.
type NativeSourceEnrollmentLocal struct {
	Identity     execenv.NativeSourceIdentity `json:"identity"`
	ManifestHash string                       `json:"manifest_hash"`
}

// nativeSourceIndexEntry is the immutable per-source profile index record.
// ManifestHash is captured from the held domain's single marker encoder at
// provision time. The index is NOT authority: the daemon loop revalidates the
// held domain's ManifestHash before any finalize, and Load reports the
// recorded hash without re-attesting the current marker.
type nativeSourceIndexEntry struct {
	SourceID     string                       `json:"source_id"`
	Identity     execenv.NativeSourceIdentity `json:"identity"`
	ManifestHash string                       `json:"manifest_hash"`
}

// nativeSourceApprovalPacket is the ephemeral inbox payload staged by the CLI.
// It carries the exact server credential plus the identity and proof the
// daemon must recheck against the physical domain before any HTTP call.
type nativeSourceApprovalPacket struct {
	Identity   execenv.NativeSourceIdentity `json:"identity"`
	Proof      NativeSourceEnrollmentProof  `json:"proof"`
	Credential SourceEnrollmentCredential   `json:"credential"`
}

// rejectTaskLocalEnrollment fails closed when the daemon's task sandbox root is
// active: provisioning must never touch or alias the global user tree from a
// task process.
func rejectTaskLocalEnrollment() error {
	if strings.TrimSpace(os.Getenv(cli.TaskConfigRootEnv)) != "" {
		return fmt.Errorf("native enrollment: unavailable under %s", cli.TaskConfigRootEnv)
	}
	return nil
}

// normalizeNativeBackendURL matches c.baseURL's trailing-slash-free shape and
// rejects anything but a plain http(s) origin-ish URL with no query, userinfo
// or fragment.
func normalizeNativeBackendURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", errors.New("native enrollment: backend URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("native enrollment: backend URL must be http(s)")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("native enrollment: backend URL must carry only scheme and host")
	}
	return trimmed, nil
}

// openPrivateRootDir opens (creating with 0700 when missing) one directory
// beneath parent and pins it with os.Root. parent must already be a strictly
// validated pinned root (never a bare path), so Mkdir cannot follow an
// aliased ancestor. A pre-existing directory must be a real directory (no
// symlink); enforcePerm additionally requires owner-only permissions and is
// set only for leaves this manager OWNS (native-sources, index, inbox) - the
// CLI's own profile parents keep their legitimate wider convention and are
// never chmod'd or rejected for it. Post-open identity is re-checked against
// the Lstat so a swapped path cannot be followed.
func openPrivateRootDir(parent *os.Root, leaf, what string, enforcePerm bool) (*os.Root, error) {
	if parent == nil {
		return nil, fmt.Errorf("native enrollment: parent root is required for %s", what)
	}
	if leaf != filepath.Base(leaf) || leaf == "" || leaf == "." || leaf == ".." {
		return nil, fmt.Errorf("native enrollment: invalid %s name %q", what, leaf)
	}
	if err := parent.Mkdir(leaf, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("native enrollment: create %s: %w", what, err)
	}
	info, err := parent.Lstat(leaf)
	if err != nil {
		return nil, fmt.Errorf("native enrollment: inspect %s: %w", what, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("native enrollment: %s must be a real directory", what)
	}
	if enforcePerm && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("native enrollment: %s must be owner-only (0700)", what)
	}
	root, err := parent.OpenRoot(leaf)
	if err != nil {
		return nil, fmt.Errorf("native enrollment: open %s: %w", what, err)
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, rootInfo) {
		root.Close()
		return nil, fmt.Errorf("native enrollment: %s changed while opening", what)
	}
	return root, nil
}

// openPrivateRootPath opens each remaining path component of base strictly
// beneath parent, so a symlinked intermediate directory (e.g. a profiles
// alias) is rejected instead of followed. These are existing CLI/user parents:
// real non-symlink directories are required, but their permissions follow the
// CLI's own convention and are not enforced here.
func openPrivateRootPath(parent *os.Root, base string, what string) (*os.Root, error) {
	if parent == nil {
		return nil, fmt.Errorf("native enrollment: parent root is required for %s", what)
	}
	rel := strings.Split(strings.Trim(filepath.ToSlash(base), "/"), "/")
	root := parent
	for _, part := range rel {
		if part == "" {
			continue
		}
		next, err := openPrivateRootDir(root, part, what, false)
		if root != parent {
			root.Close()
		}
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

// openProfileBaseRoot opens the real ProfileDir(profile) tree from the user's
// home directory: every component from HOME down must be a real non-symlink
// directory (an alias is rejected), but permissions follow the CLI convention.
func openProfileBaseRoot(profile string) (*os.Root, error) {
	base, err := cli.ProfileDir(profile)
	if err != nil {
		return nil, fmt.Errorf("native enrollment: resolve profile dir: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("native enrollment: resolve home: %w", err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		return nil, fmt.Errorf("native enrollment: open home: %w", err)
	}
	defer homeRoot.Close()
	rel, err := filepath.Rel(home, base)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("native enrollment: profile dir escapes home")
	}
	return openPrivateRootPath(homeRoot, rel, "profile base")
}

// openNativeSourcesRoot opens the pinned machine-global native-sources root.
// All physical domain access goes through the returned os.Root; no path flags
// are accepted from callers.
func openNativeSourcesRoot() (*os.Root, error) {
	baseRoot, err := openProfileBaseRoot("")
	if err != nil {
		return nil, err
	}
	defer baseRoot.Close()
	// native-sources is owned by this manager: strict owner-only enforcement.
	return openPrivateRootDir(baseRoot, nativeSourcesDir, "global root", true)
}

// openProfileEnrollmentDir opens a private per-profile subdirectory of
// ProfileDir(profile)/native-source-enrollments (plus its inbox leaf). The
// inbox is opened THROUGH the strictly validated index root, so a symlinked or
// public native-source-enrollments parent cannot hide behind it.
func openProfileEnrollmentDir(profile, leaf string) (*os.Root, error) {
	if err := validateProfileName(profile); err != nil {
		return nil, err
	}
	if leaf != "" && leaf != nativeSourceInboxDir {
		return nil, fmt.Errorf("native enrollment: unknown profile dir %q", leaf)
	}
	baseRoot, err := openProfileBaseRoot(profile)
	if err != nil {
		return nil, err
	}
	defer baseRoot.Close()
	// Index and inbox are owned by this manager: strict owner-only enforcement.
	indexRoot, err := openPrivateRootDir(baseRoot, nativeSourceIndexDir, "profile dir", true)
	if err != nil {
		return nil, err
	}
	if leaf == nativeSourceInboxDir {
		inboxRoot, err := openPrivateRootDir(indexRoot, nativeSourceInboxDir, "inbox", true)
		indexRoot.Close()
		if err != nil {
			return nil, err
		}
		return inboxRoot, nil
	}
	return indexRoot, nil
}

// validateProfileName keeps the profile from escaping its directory.
func validateProfileName(profile string) error {
	if profile == "" {
		return nil
	}
	if profile != filepath.Base(profile) || profile == "." || profile == ".." ||
		strings.ContainsAny(profile, `/\:`) || strings.TrimSpace(profile) == "" {
		return fmt.Errorf("native enrollment: invalid profile name")
	}
	return nil
}

// nativeIdentityFromEnrollment derives the physical-domain identity from a
// server enrollment record and the caller's backend URL. The marker's
// EnrollmentID is the server nonce; BackendURL is normalized here so the same
// spelling always yields the same marker (and hash).
func nativeIdentityFromEnrollment(profile, backendURL string, source NativeSourceEnrollment) (execenv.NativeSourceIdentity, string, error) {
	normalized, err := normalizeNativeBackendURL(backendURL)
	if err != nil {
		return execenv.NativeSourceIdentity{}, "", err
	}
	id := execenv.NativeSourceIdentity{
		BackendURL:   normalized,
		WorkspaceID:  source.WorkspaceID,
		SourceID:     source.ID,
		RuntimeID:    source.RuntimeID,
		DaemonID:     source.DaemonID,
		EnrollmentID: source.NativeEnrollmentID,
	}
	if err := validateNativeEnrollmentSelectors(source.RuntimeID, source.WorkspaceID, source.DaemonID); err != nil {
		return execenv.NativeSourceIdentity{}, "", err
	}
	if err := validateSourceReadUUID("source id", source.ID); err != nil {
		return execenv.NativeSourceIdentity{}, "", err
	}
	if err := validateSourceReadUUID("enrollment id", source.NativeEnrollmentID); err != nil {
		return execenv.NativeSourceIdentity{}, "", err
	}
	return id, profile, nil
}

// readStrictRootFile reads one bounded strict JSON object through a pinned
// root. The file must be a regular non-symlink file with owner-only
// permissions, and the opened handle must be the same file Lstat saw.
func readStrictRootFile(root *os.Root, name string, dest any) error {
	lstat, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("native enrollment: inspect %s: %w", name, err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 || !lstat.Mode().IsRegular() {
		return fmt.Errorf("native enrollment: %s is not a regular file", name)
	}
	if lstat.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("native enrollment: %s must be owner-only (0600)", name)
	}
	f, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("native enrollment: open %s: %w", name, err)
	}
	defer f.Close()
	openStat, err := f.Stat()
	if err != nil || !os.SameFile(lstat, openStat) {
		return fmt.Errorf("native enrollment: %s changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, nativeSourceFileMax+1))
	if err != nil {
		return fmt.Errorf("native enrollment: read %s: %w", name, err)
	}
	if len(data) > nativeSourceFileMax {
		return fmt.Errorf("native enrollment: %s exceeds %d bytes", name, nativeSourceFileMax)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("native enrollment: decode %s", name)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("native enrollment: trailing data in %s", name)
	}
	return nil
}

// writeStrictRootFile publishes one JSON object atomically through a pinned
// root with a random temporary name, then links it onto final via
// root.Link: link(2) fails when final already exists, so publication is
// create-only even against a concurrent writer. Mode 0600; fsync before link.
func writeStrictRootFile(root *os.Root, final string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > nativeSourceFileMax {
		return fmt.Errorf("native enrollment: encoded file exceeds %d bytes", nativeSourceFileMax)
	}
	tmp := fmt.Sprintf(".tmp-%s", uuid.NewString())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("native enrollment: create temp file: %w", err)
	}
	fail := func(e error) error {
		f.Close()
		root.Remove(tmp)
		return e
	}
	if _, err := f.Write(encoded); err != nil {
		return fail(fmt.Errorf("native enrollment: write temp file: %w", err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("native enrollment: sync temp file: %w", err))
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp)
		return err
	}
	// Link is create-only: it fails with EEXIST when final already exists,
	// making publication atomic against concurrent writers without a TOCTOU
	// Lstat window. The hard link means the packet also survives later
	// Remove(tmp) calls on other paths.
	if err := root.Link(tmp, final); err != nil {
		return fail(fmt.Errorf("native enrollment: publish %s: %w", final, err))
	}
	root.Remove(tmp)
	return nil
}

// ProvisionNativeSourceLocal creates the fresh physical domain for source,
// publishes the profile index entry, and returns. The source owner lock is
// held across validation and publication, then released: the daemon (not the
// CLI) retains it after restart.
func ProvisionNativeSourceLocal(profile, backendURL string, source NativeSourceEnrollment) error {
	if err := rejectTaskLocalEnrollment(); err != nil {
		return err
	}
	id, _, err := nativeIdentityFromEnrollment(profile, backendURL, source)
	if err != nil {
		return err
	}
	if err := validateNativeEnrollmentSource(&source, source.RuntimeID, source.WorkspaceID, source.DaemonID); err != nil {
		return err
	}
	if source.NativeEnrollmentStatus != "pending" && source.NativeEnrollmentStatus != "enrolled" {
		return fmt.Errorf("native enrollment: unknown enrollment status")
	}
	// enabled is only reachable for approved sources, which require an exact
	// existing domain/index below; a pending+enabled or missing-domain replay
	// fails closed there.
	approved := source.NativeEnrollmentStatus == "enrolled" || source.NativeManifestHash != ""
	if source.Enabled && !approved {
		return fmt.Errorf("native enrollment: enabled source requires an enrolled replay")
	}

	root, err := openNativeSourcesRoot()
	if err != nil {
		return err
	}
	defer root.Close()

	// Retry window: when an existing identical index exists and the domain is
	// currently held (daemon live), the provision is a no-op as long as the
	// recorded identity matches exactly. Any mismatch fails.
	indexRoot, err := openProfileEnrollmentDir(profile, "")
	if err != nil {
		return err
	}
	defer indexRoot.Close()
	existing, err := readNativeSourceIndexEntry(indexRoot, id.SourceID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A torn or corrupt index fails closed: it is not repairable here.
		return err
	}
	if existing != nil && existing.Identity != id {
		return fmt.Errorf("native enrollment: source %s is already indexed with a different identity", id.SourceID)
	}
	// Any pinned server hash (approved pending OR enrolled) must agree with
	// the recorded index hash before any no-op return: the index is not
	// attestation, but it must not contradict the server's pinned H.
	if source.NativeManifestHash != "" && existing != nil && existing.ManifestHash != source.NativeManifestHash {
		return fmt.Errorf("native enrollment: server manifest hash does not match local index for source %s", id.SourceID)
	}
	if approved && existing == nil {
		return fmt.Errorf("native enrollment: approved source %s has no local domain; refusing fresh creation", id.SourceID)
	}

	var domain *execenv.NativeSourceDomain
	if approved {
		// An approved/enrolled source must already exist physically: strict
		// open only, never fresh creation.
		domain, err = execenv.OpenNativeSource(root, id)
		if err != nil {
			if errors.Is(err, execenv.ErrNativeSourceBusy) {
				// Daemon holds the exact domain; the index already records this
				// identity, so the replay changes nothing.
				return nil
			}
			return fmt.Errorf("native enrollment: open enrolled source %s: %w", id.SourceID, err)
		}
	} else {
		domain, err = execenv.CreateNativeSource(root, id)
		if err != nil {
			if errors.Is(err, execenv.ErrNativeSourceBusy) && existing != nil {
				// Daemon holds the exact domain and the index already records this
				// identity: replaying provision changes nothing.
				return nil
			}
			if !isNativeSourceAlreadyExists(err) {
				return err
			}
			// Domain already exists: strict reopen, complete marker only. The
			// marker must match this identity exactly; anything else fails closed.
			domain, err = execenv.OpenNativeSource(root, id)
			if err != nil {
				if errors.Is(err, execenv.ErrNativeSourceBusy) && existing != nil {
					return nil
				}
				return fmt.Errorf("native enrollment: reopen source %s: %w", id.SourceID, err)
			}
		}
	}
	defer domain.Close()

	hash, err := domain.ManifestHash()
	if err != nil {
		return err
	}
	// Same agreement against the held marker hash, before the idle no-op
	// return and before publishing: approved pending OR enrolled alike.
	if source.NativeManifestHash != "" && source.NativeManifestHash != hash {
		return fmt.Errorf("native enrollment: server manifest hash does not match local domain for source %s", id.SourceID)
	}
	if existing != nil {
		// Identical index already published. The recorded hash must equal the
		// currently held marker's hash; otherwise the domain changed.
		if existing.ManifestHash != hash {
			return fmt.Errorf("native enrollment: indexed hash does not match local domain for source %s", id.SourceID)
		}
		return nil
	}
	if err := writeStrictRootFile(indexRoot, nativeSourceIndexName(id.SourceID), nativeSourceIndexEntry{
		SourceID:     id.SourceID,
		Identity:     id,
		ManifestHash: hash,
	}); err != nil {
		return err
	}
	return nil
}

// isNativeSourceAlreadyExists reports whether err is the Mkdir EEXIST from
// CreateNativeSource (the leaf already existed).
func isNativeSourceAlreadyExists(err error) bool {
	var perr *os.PathError
	return errors.As(err, &perr) && errors.Is(perr.Err, os.ErrExist)
}

func nativeSourceIndexName(sourceID string) string { return sourceID + ".json" }

// readNativeSourceIndexEntry returns (nil, nil) when the index entry is
// absent.
func readNativeSourceIndexEntry(root *os.Root, sourceID string) (*nativeSourceIndexEntry, error) {
	var entry nativeSourceIndexEntry
	err := readStrictRootFile(root, nativeSourceIndexName(sourceID), &entry)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if entry.SourceID != sourceID {
		return nil, fmt.Errorf("native enrollment: index entry for %s names %s", sourceID, entry.SourceID)
	}
	if err := validateNativeEnrollmentSelectors(entry.Identity.RuntimeID, entry.Identity.WorkspaceID, entry.Identity.DaemonID); err != nil {
		return nil, fmt.Errorf("native enrollment: index entry identity: %w", err)
	}
	if err := validateSourceReadUUID("source id", entry.Identity.SourceID); err != nil {
		return nil, err
	}
	if entry.Identity.SourceID != sourceID {
		return nil, fmt.Errorf("native enrollment: index identity source mismatch")
	}
	return &entry, nil
}

// LoadNativeSourceEnrollmentLocal reads the immutable profile index entry and
// returns the recorded identity and manifest hash. It takes NO source owner
// lock and performs no HTTP: the CLI calls it while the daemon holds the
// domain, and the index is not authority - the daemon revalidates the held
// domain's marker hash against any packet before finalizing.
func LoadNativeSourceEnrollmentLocal(profile, backendURL, workspaceID, sourceID string) (NativeSourceEnrollmentLocal, error) {
	var zero NativeSourceEnrollmentLocal
	if err := rejectTaskLocalEnrollment(); err != nil {
		return zero, err
	}
	normalized, err := normalizeNativeBackendURL(backendURL)
	if err != nil {
		return zero, err
	}
	if err := validateSourceReadIDs(map[string]string{"workspace id": workspaceID, "source id": sourceID}); err != nil {
		return zero, err
	}
	indexRoot, err := openProfileEnrollmentDir(profile, "")
	if err != nil {
		return zero, err
	}
	defer indexRoot.Close()
	entry, err := readNativeSourceIndexEntry(indexRoot, sourceID)
	if err != nil {
		return zero, err
	}
	if entry == nil {
		return zero, fmt.Errorf("native enrollment: no local enrollment for source %s", sourceID)
	}
	if entry.Identity.BackendURL != normalized || entry.Identity.WorkspaceID != workspaceID {
		return zero, fmt.Errorf("native enrollment: local enrollment for source %s does not match request", sourceID)
	}
	if err := validateNativeManifestHash(entry.ManifestHash); err != nil {
		return zero, fmt.Errorf("native enrollment: index entry hash: %w", err)
	}
	return NativeSourceEnrollmentLocal{Identity: entry.Identity, ManifestHash: entry.ManifestHash}, nil
}

// validateNativeApprovalPacket checks the staged packet shape: mse token,
// bounded hashes, positive short expiry, and selector agreement between the
// credential, proof and identity.
func validateNativeApprovalPacket(p *nativeSourceApprovalPacket) error {
	if err := validateNativeEnrollmentSelectors(p.Identity.RuntimeID, p.Identity.WorkspaceID, p.Identity.DaemonID); err != nil {
		return err
	}
	if err := validateSourceReadUUID("source id", p.Identity.SourceID); err != nil {
		return err
	}
	if err := validateSourceReadUUID("enrollment id", p.Identity.EnrollmentID); err != nil {
		return err
	}
	if p.Identity.EnrollmentID != p.Credential.EnrollmentID || p.Identity.EnrollmentID != p.Proof.EnrollmentID {
		return fmt.Errorf("native enrollment: packet enrollment ids disagree")
	}
	if p.Identity.SourceID != p.Credential.SourceID {
		return fmt.Errorf("native enrollment: packet source ids disagree")
	}
	if p.Identity.WorkspaceID != p.Credential.WorkspaceID || p.Identity.RuntimeID != p.Credential.RuntimeID || p.Identity.DaemonID != p.Credential.DaemonID {
		return fmt.Errorf("native enrollment: packet selectors disagree")
	}
	if err := validateNativeEnrollmentProof(p.Proof); err != nil {
		return err
	}
	if p.Proof.ConfigRevision != p.Credential.ConfigRevision || p.Proof.ManifestHash != p.Credential.ManifestHash {
		return fmt.Errorf("native enrollment: packet proof disagrees with credential")
	}
	if err := requireMSEToken(p.Credential.Token); err != nil {
		return err
	}
	if err := validateNativeManifestHash(p.Credential.ManifestHash); err != nil {
		return err
	}
	if p.Credential.ExpiresAt.IsZero() || !p.Credential.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("native enrollment: packet credential is expired")
	}
	if p.Credential.ExpiresIn <= 0 || p.Credential.ExpiresIn > 120 {
		return fmt.Errorf("native enrollment: packet credential lifetime must be within 1..120 seconds")
	}
	return nil
}

// StageNativeSourceEnrollmentApproval publishes one approval packet for the
// daemon to consume. It deliberately does NOT require the source owner lock:
// the daemon normally holds it. The unique staged filename
// (SourceID.<nonce>.json) means a response-loss retry or a stale ack can never
// delete a newer approval for the same source.
func StageNativeSourceEnrollmentApproval(profile, backendURL string, source NativeSourceEnrollment, credential SourceEnrollmentCredential) error {
	if err := rejectTaskLocalEnrollment(); err != nil {
		return err
	}
	id, _, err := nativeIdentityFromEnrollment(profile, backendURL, source)
	if err != nil {
		return err
	}
	packet := nativeSourceApprovalPacket{
		Identity:   id,
		Proof:      NativeSourceEnrollmentProof{EnrollmentID: credential.EnrollmentID, ConfigRevision: credential.ConfigRevision, ManifestHash: credential.ManifestHash},
		Credential: credential,
	}
	if err := validateNativeApprovalPacket(&packet); err != nil {
		return err
	}
	inbox, err := openProfileEnrollmentDir(profile, nativeSourceInboxDir)
	if err != nil {
		return err
	}
	defer inbox.Close()
	name := fmt.Sprintf("%s.%s.json", id.SourceID, uuid.NewString())
	if err := writeStrictRootFile(inbox, name, &packet); err != nil {
		return err
	}
	return nil
}

// heldNativeSource is one domain retained by the daemon loop.
type heldNativeSource struct {
	domain  *execenv.NativeSourceDomain
	entry   nativeSourceIndexEntry
	profile string
}

// nativeSourceEnrollmentLoop reconciles staged approvals into finalized
// enrollments. It enumerates ONLY this daemon's profile indices whose recorded
// backend URL and daemon ID match the running configuration, and only sources
// whose workspace/runtime pairing is tracked by this daemon. Each held domain
// keeps its owner lock until the loop's context is cancelled, even if its
// index entry disappears meanwhile.
// ponytail: serial finalize retries can delay other sources by one capability
// lifetime (120s). Use a bounded per-source worker pool if enrollment volume grows.
func (d *Daemon) nativeSourceEnrollmentLoop(ctx context.Context) {
	held := make(map[string]*heldNativeSource) // sourceID -> held
	defer func() {
		for _, h := range held {
			h.domain.Close()
		}
	}()
	var timer *time.Timer
	for {
		timer = time.NewTimer(nativeEnrollmentLoopPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		d.nativeSourceEnrollmentReconcile(ctx, held)
	}
}

// nativeSourceTrackedRuntime reports whether this daemon currently tracks the
// EXACT workspace -> runtime pairing: the runtime must be in the workspace's
// runtimeIDs AND live in the runtime index. A runtime present only in the
// global index (removed from this workspace) is not a pairing.
func (d *Daemon) nativeSourceTrackedRuntime(workspaceID, runtimeID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, ok := d.workspaces[workspaceID]
	if !ok {
		return false
	}
	paired := false
	for _, rid := range ws.runtimeIDs {
		if rid == runtimeID {
			paired = true
			break
		}
	}
	if !paired {
		return false
	}
	_, live := d.runtimeIndex[runtimeID]
	return live
}

// nativeSourceProfiles returns the profile names to scan. One daemon process
// serves exactly its own profile.
func (d *Daemon) nativeSourceProfiles() []string {
	profile := d.cfg.Profile
	if err := validateProfileName(profile); err != nil {
		return nil
	}
	return []string{profile}
}

// readRootDirNames lists the non-hidden .json entries of a pinned root. A
// dedicated helper because os.Root has no ReadDir: it opens the directory
// through the root, reads names with ReadDir(-1), then closes.
func readRootDirNames(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	dirents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(dirents))
	for _, e := range dirents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// nativeSourceEnrollmentReconcile runs one pass: adopt newly indexed sources,
// then drain each held source's inbox.
func (d *Daemon) nativeSourceEnrollmentReconcile(ctx context.Context, held map[string]*heldNativeSource) {
	if strings.TrimSpace(os.Getenv(cli.TaskConfigRootEnv)) != "" {
		return
	}
	cfgBackend, err := normalizeNativeBackendURL(d.cfg.ServerBaseURL)
	if err != nil || strings.TrimSpace(d.cfg.DaemonID) == "" {
		return
	}
	for _, profile := range d.nativeSourceProfiles() {
		indexRoot, err := openProfileEnrollmentDir(profile, "")
		if err != nil {
			continue
		}
		entries, err := readRootDirNames(indexRoot)
		indexRoot.Close()
		if err != nil {
			continue
		}
		for _, name := range entries {
			sourceID := strings.TrimSuffix(name, ".json")
			if _, already := held[sourceID]; already {
				continue
			}
			d.nativeSourceAdopt(cfgBackend, profile, sourceID, held)
		}
	}
	for _, h := range held {
		d.nativeSourceDrainInbox(ctx, cfgBackend, h)
	}
}

// nativeSourceAdopt validates one index entry and acquires the domain owner
// lock for the daemon. Adoption fails closed on any marker/identity mismatch
// or untracked workspace/runtime pairing; nothing is reset or deleted.
func (d *Daemon) nativeSourceAdopt(cfgBackend, profile, sourceID string, held map[string]*heldNativeSource) {
	indexRoot, err := openProfileEnrollmentDir(profile, "")
	if err != nil {
		return
	}
	defer indexRoot.Close()
	entry, err := readNativeSourceIndexEntry(indexRoot, sourceID)
	if err != nil || entry == nil {
		return
	}
	if entry.Identity.BackendURL != cfgBackend || entry.Identity.DaemonID != d.cfg.DaemonID {
		return
	}
	if !d.nativeSourceTrackedRuntime(entry.Identity.WorkspaceID, entry.Identity.RuntimeID) {
		return
	}
	root, err := openNativeSourcesRoot()
	if err != nil {
		return
	}
	defer root.Close()
	domain, err := execenv.OpenNativeSource(root, entry.Identity)
	if err != nil {
		// Busy means another process holds it (e.g. the CLI during provision);
		// any invalid marker fails closed. Neither is retried destructively.
		return
	}
	held[sourceID] = &heldNativeSource{domain: domain, entry: *entry, profile: profile}
}

// nativeSourceDrainInbox consumes approval packets for one held domain. Each
// unique inbox file is removed only on an exact acknowledged finalize; expired
// or definitely rejected (4xx) packets are removed with a fixed warning that
// names only the source ID. The held domain is NEVER released here: locks
// stay held until the loop's context is cancelled.
func (d *Daemon) nativeSourceDrainInbox(ctx context.Context, cfgBackend string, h *heldNativeSource) {
	inbox, err := openProfileEnrollmentDir(h.profile, nativeSourceInboxDir)
	if err != nil {
		return
	}
	defer inbox.Close()
	names, err := readRootDirNames(inbox)
	if err != nil {
		return
	}
	hash, err := h.domain.ManifestHash()
	if err != nil {
		// Marker unreadable or changed: finalization fails closed, but the
		// domain lock stays held until the loop's context is cancelled - never
		// dropped early. The packet stays in the inbox for operator visibility.
		d.logger.Warn("native enrollment: domain marker unreadable; finalize blocked, domain still held", "source_id", h.entry.Identity.SourceID)
		return
	}
	for _, name := range names {
		if !strings.HasPrefix(name, h.entry.Identity.SourceID+".") {
			continue
		}
		var packet nativeSourceApprovalPacket
		if err := readStrictRootFile(inbox, name, &packet); err != nil {
			// Corrupt packet: remove only the ephemeral file.
			inbox.Remove(name)
			continue
		}
		if err := d.nativeSourceFinalizePacket(ctx, cfgBackend, h, hash, &packet); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isNativeFinalizeTransient(err) {
				// Retry later with the same file while the credential is unexpired.
				continue
			}
			// Mismatch, expiry or definite rejection: remove ONLY the ephemeral
			// packet. Marker, domain and index are untouched; a fixed warning
			// already named only the source ID.
			inbox.Remove(name)
			continue
		}
		// Exact ack for THIS unique file only.
		if err := inbox.Remove(name); err != nil {
			continue
		}
	}
}

// nativeSourceFinalizePacket rechecks everything against the held domain and
// the packet, then drives finalize with retries. It reports nil only after an
// acknowledged enrolled receipt.
func (d *Daemon) nativeSourceFinalizePacket(ctx context.Context, cfgBackend string, h *heldNativeSource, domainHash string, p *nativeSourceApprovalPacket) error {
	id := h.entry.Identity
	if p.Identity != id {
		d.logger.Warn("native enrollment: approval packet does not match held domain; rerun approve", "source_id", id.SourceID)
		return errNativePacketMismatch
	}
	if p.Credential.ManifestHash != domainHash || p.Proof.ManifestHash != domainHash {
		d.logger.Warn("native enrollment: approval packet does not match held domain; rerun approve", "source_id", id.SourceID)
		return errNativePacketMismatch
	}
	if p.Identity.BackendURL != cfgBackend {
		return errNativePacketMismatch
	}
	if err := validateNativeApprovalPacket(p); err != nil {
		return errNativePacketMismatch
	}
	proof := p.Proof
	// Deadline bounded by the credential expiry; no fallback credential.
	deadline := time.Now().Add(nativeEnrollmentMaxTTL)
	if p.Credential.ExpiresAt.Before(deadline) {
		deadline = p.Credential.ExpiresAt
	}
	for {
		if time.Now().After(deadline) {
			d.logger.Warn("native enrollment: approval expired before finalize; rerun approve", "source_id", id.SourceID)
			return errNativePacketExpired
		}
		callCtx, cancel := context.WithDeadline(ctx, deadline)
		_, err := d.client.FinalizeNativeSourceEnrollment(callCtx, id.RuntimeID, id.WorkspaceID, id.DaemonID, id.SourceID, p.Credential.Token, proof)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isNativeFinalizeTransient(err) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nativeEnrollmentRetryPause):
			}
			continue
		}
		d.logger.Warn("native enrollment: finalize rejected; rerun approve for a fresh capability", "source_id", id.SourceID)
		return err
	}
}

var (
	errNativePacketMismatch = errors.New("native enrollment: approval packet does not match held domain")
	errNativePacketExpired  = errors.New("native enrollment: approval expired")
)

// isNativeFinalizeTransient retries only transport failures, 408, 429 and 5xx.
func isNativeFinalizeTransient(err error) bool {
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		return reqErr.StatusCode == http.StatusRequestTimeout ||
			reqErr.StatusCode == http.StatusTooManyRequests ||
			reqErr.StatusCode >= 500
	}
	// Transport-level (URL, deadline, connection) errors carry no status.
	var perr *url.Error
	if errors.As(err, &perr) {
		return true
	}
	return false
}
