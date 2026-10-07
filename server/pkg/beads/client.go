// Package beads provides a read-only client for a Beads source through the
// pinned `bd` CLI JSON output (bd 1.3.x, embedded Dolt mode).
//
// Contract (qualified against bd 1.3.1 with a disposable embedded-Dolt
// source; see qualification report referenced from T01):
//
//   - list: `bd list --json --flat -n <limit>` prints a JSON array of issue
//     summaries. The default list shape omits empty text fields; full text
//     must be fetched per issue with show.
//   - show: `bd show --id=<id> --json` prints a JSON array with one issue
//     (full detail, includes `revision`). A not-found ID exits 1 with a
//     JSON error object on stdout.
//   - raw outgoing edges: `bd --readonly --sandbox dep list --direction down
//     --json -- <id> <id>` prints raw issue_id/depends_on_id/type records,
//     including dangling/external endpoints. Repeated anchors select batch
//     mode. Nonempty stderr, including missing-anchor warnings, is failure.
//
// Write qualification result (bd 1.3.1): `bd update` supports status and
// assignee preconditions (`--if-status`, `--if-assignee`, exit 13 on stale
// guard) but no revision precondition, and `bd create` cannot set or return
// attribution/revision atomically. This package therefore exposes reads
// only; do not add writes without a supported conditional-write path.
//
// The native ID (e.g. `bd-1a2b`) is opaque and passed through verbatim; no
// shell is ever involved. Decoded payloads are untrusted input: strict
// structs, unknown fields discarded, command output read bounded.
package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/redact"
)

// maxOutputBytes caps bd stdout/stderr capture so a pathological source
// cannot exhaust daemon memory.
const maxOutputBytes = 4 << 20

// MaxDependencies bounds one complete outgoing edge observation.
const MaxDependencies = 512

// defaultTimeout bounds every bd invocation when the caller supplies a
// background context; a caller deadline always wins.
var defaultTimeout = 30 * time.Second

// Client reads one Beads source via the configured `bd` executable.
// It is safe for concurrent use.
type Client struct {
	// Executable is the operator-configured bd binary path or name.
	Executable string
	// BeadsDir is the approved source `.beads` directory, passed to the
	// command via BEADS_DIR. Empty is rejected at the run boundary:
	// bd would otherwise auto-discover an unapproved source from cwd.
	BeadsDir string
}

// IssueSummary is one row of `bd list --json` output. Text fields omitted
// by the brief list shape decode as absent (nil pointers).
type IssueSummary struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Description     *string `json:"description"`
	Status          string  `json:"status"`
	Priority        int     `json:"priority"`
	IssueType       string  `json:"issue_type"`
	Owner           *string `json:"owner"`
	CreatedAt       string  `json:"created_at"`
	CreatedBy       *string `json:"created_by"`
	UpdatedAt       string  `json:"updated_at"`
	DependencyCount int     `json:"dependency_count"`
	DependentCount  int     `json:"dependent_count"`
	CommentCount    int     `json:"comment_count"`
}

// Issue is one element of `bd show --id=<id> --json` output.
type Issue struct {
	IssueSummary
	// Revision is an item revision, not dependency topology CAS or an atomic snapshot.
	Revision     string       `json:"revision"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
	// Complete only for the separate raw edge read, not atomic with metadata
	// or other items. Revision is never dependency topology CAS.
	DependenciesComplete bool `json:"dependencies_complete,omitempty"`
}

// MarshalJSON keeps legacy receipt bytes unchanged while complete empty edge
// observations explicitly carry [] rather than omitted or null dependencies.
func (issue Issue) MarshalJSON() ([]byte, error) {
	type plain Issue
	if !issue.DependenciesComplete {
		return json.Marshal(plain(issue))
	}
	dependencies := issue.Dependencies
	if dependencies == nil {
		dependencies = []Dependency{}
	}
	return json.Marshal(struct {
		plain
		Dependencies []Dependency `json:"dependencies"`
	}{plain(issue), dependencies})
}

// Dependency retains the opaque predecessor endpoint and exact relation kind.
// External endpoints and unknown/nonblocking kinds are data, not execution gates.
type Dependency struct {
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
}

// ErrNotFound reports that the source has no issue with the native ID.
var ErrNotFound = errors.New("beads: issue not found")

// List returns up to limit issue summaries. limit <= 0 uses the CLI default
// (50); callers wanting full scans must pass an explicit large limit.
func (c *Client) List(ctx context.Context, limit int) ([]IssueSummary, error) {
	args := []string{"list", "--json", "--flat"}
	if limit > 0 {
		args = append(args, "-n", fmt.Sprintf("%d", limit))
	}
	var rows []IssueSummary
	if err := c.runJSON(ctx, args, &rows, false); err != nil {
		return nil, err
	}
	// Payloads are untrusted source output: an empty or duplicated native ID
	// must fail closed instead of populating a wrong scoped work card.
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.ID) == "" {
			return nil, fmt.Errorf("beads list: row with missing id")
		}
		if _, dup := seen[row.ID]; dup {
			return nil, fmt.Errorf("beads list: duplicate id %q", row.ID)
		}
		seen[row.ID] = struct{}{}
	}
	return rows, nil
}

// ReadTask returns the full issue for a native ID.
func (c *Client) ReadTask(ctx context.Context, nativeID string) (Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	if strings.TrimSpace(nativeID) == "" {
		return Issue{}, fmt.Errorf("beads: empty native ID")
	}
	var rows []Issue
	if err := c.runJSON(ctx, []string{"show", "--id=" + nativeID, "--json"}, &rows, false); err != nil {
		return Issue{}, err
	}
	switch len(rows) {
	case 1:
		issue := rows[0]
		// Trust boundary: the returned row must answer exactly the ID we
		// asked for and carry the detail-only revision field.
		if issue.ID != nativeID {
			return Issue{}, fmt.Errorf("beads: show %q returned id %q", nativeID, issue.ID)
		}
		if strings.TrimSpace(issue.Revision) == "" {
			return Issue{}, fmt.Errorf("beads: show %q returned no revision", nativeID)
		}
		// Show silently drops failed/dangling dependency reads. Repeated exact
		// anchors select bd 1.3.1's public raw batch operation instead of hydrated
		// neighbors. Any warnings (including missing anchors) fail closed.
		issue.DependenciesComplete = false
		var edges []struct {
			IssueID     string `json:"issue_id"`
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
		}
		args := []string{"--readonly", "--sandbox", "dep", "list", "--direction", "down", "--json", "--", nativeID, nativeID}
		if err := c.runJSON(ctx, args, &edges, true); err != nil {
			return Issue{}, err
		}
		if edges == nil || len(edges) > MaxDependencies {
			return Issue{}, errors.New("beads: invalid raw dependency array")
		}
		dependencies := make([]Dependency, 0, len(edges))
		seen := make(map[Dependency]bool, len(edges))
		for _, edge := range edges {
			dependency := Dependency{ID: edge.DependsOnID, DependencyType: edge.Type}
			if edge.IssueID != nativeID || strings.TrimSpace(dependency.ID) == "" || strings.TrimSpace(dependency.DependencyType) == "" || seen[dependency] {
				return Issue{}, errors.New("beads: invalid raw dependency edge")
			}
			seen[dependency] = true
			dependencies = append(dependencies, dependency)
		}
		issue.Dependencies = dependencies
		issue.DependencyCount = len(dependencies)
		issue.DependenciesComplete = true
		return issue, nil
	case 0:
		return Issue{}, fmt.Errorf("%w: %s", ErrNotFound, nativeID)
	default:
		// Multiple rows for a single-ID show violate the source contract.
		// Fail closed rather than guess a row.
		return Issue{}, fmt.Errorf("beads: show %q returned %d issues, want 1", nativeID, len(rows))
	}
}

// runError mirrors bd's `--json` error object shape:
// {"error": string, "hint": string, "schema_version": number}.
type runError struct {
	Error         string `json:"error"`
	Hint          string `json:"hint"`
	SchemaVersion int    `json:"schema_version"`
}

// runJSON executes bd with argv args (no shell) against the approved source
// directory and decodes its bounded stdout as JSON.
func (c *Client) runJSON(ctx context.Context, args []string, dst any, rejectStderr bool) error {
	if c.Executable == "" {
		return fmt.Errorf("beads: no executable configured")
	}
	if c.BeadsDir == "" {
		// Empty is rejected, not defaulted: bd would auto-discover a `.beads`
		// directory from the process cwd, an unapproved source. The caller
		// must name the approved directory explicitly.
		return fmt.Errorf("beads: no source directory configured")
	}
	// Always bound the invocation: WithTimeout preserves an earlier
	// deadline and installs the default when the caller set none, so a
	// far-future caller deadline cannot disable the cap.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Executable, args...)
	// After a context kill, a grandchild can still hold the output pipe;
	// abandon the pipes instead of waiting for it forever.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(cmd.Environ(), "BEADS_DIR="+c.BeadsDir)
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// Raw edge errors must not expose argv, executable/source paths, source
	// diagnostics or decode details. Even exit-zero whitespace stderr fails.
	if rejectStderr {
		if ctx.Err() != nil {
			return fmt.Errorf("beads: raw dependency read: %w", ctx.Err())
		}
		if err != nil || stdout.overflow(&stderr) != nil || len(stderr.Bytes()) != 0 {
			return errors.New("beads: raw dependency read failed")
		}
		raw := bytes.TrimSpace(stdout.Bytes())
		if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, dst) != nil {
			return errors.New("beads: invalid raw dependency payload")
		}
		return nil
	}
	// An output-cap overflow must win over the command's own exit error:
	// the payload is untrustworthy either way.
	if oerr := stdout.overflow(&stderr); oerr != nil {
		return fmt.Errorf("beads %s: %w", args[0], oerr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("beads %s: %w (ctx: %v)", args[0], err, ctx.Err())
		}
		return decodeRunError(args, stdout.Bytes(), stderr.Bytes(), err)
	}
	if bytes.Equal(bytes.TrimSpace(stdout.Bytes()), []byte("null")) {
		return fmt.Errorf("beads %s: expected a JSON array, got null", args[0])
	}
	if err := json.Unmarshal(stdout.Bytes(), dst); err != nil {
		return fmt.Errorf("beads %s: decode payload: %w", args[0], err)
	}
	return nil
}

// decodeRunError classifies a nonzero bd exit. bd prints structured JSON
// errors on stdout for known failures (e.g. not-found); anything else is a
// generic exec failure with bounded stderr context.
func decodeRunError(args []string, stdout, stderr []byte, err error) error {
	var re runError
	if json.Unmarshal(stdout, &re) == nil && re.Error != "" {
		if args[0] == "show" && strings.Contains(re.Error, "no issues found") {
			return fmt.Errorf("%w: %s", ErrNotFound, strings.TrimPrefix(args[1], "--id="))
		}
		msg := truncateDetail(redact.Text(re.Error))
		if re.Hint != "" {
			msg += " (" + truncateDetail(redact.Text(re.Hint)) + ")"
		}
		return fmt.Errorf("beads %s: %s", args[0], msg)
	}
	detail := redact.Text(strings.TrimSpace(string(stderr)))
	if detail == "" {
		detail = redact.Text(strings.TrimSpace(string(stdout)))
	}
	detail = truncateDetail(detail)
	if detail == "" {
		return fmt.Errorf("beads %s: %w", args[0], err)
	}
	return fmt.Errorf("beads %s: %w: %s", args[0], err, detail)
}

// truncateDetail bounds any diagnostic text included in an error so a
// hostile or broken bd cannot balloon error strings. Applied to structured
// fields and fallback output alike.
func truncateDetail(s string) string {
	if len(s) > 512 {
		return s[:512]
	}
	return s
}

// limitedBuffer is a bytes.Buffer that stops accepting writes past cap
// bytes, so oversized command output cannot accumulate unbounded.
type limitedBuffer struct {
	buf      bytes.Buffer
	overrun  error
	combined int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.combined += len(p)
	if b.overrun == nil && b.combined > maxOutputBytes {
		b.overrun = fmt.Errorf("command output exceeds %d bytes", maxOutputBytes)
	}
	if b.overrun != nil {
		// Returning an error stops exec's copy goroutine; the child then
		// dies on SIGPIPE instead of being drained forever.
		return 0, b.overrun
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// overflow returns the overrun error of either buffer, if any.
func (b *limitedBuffer) overflow(other *limitedBuffer) error {
	if b.overrun != nil {
		return b.overrun
	}
	return other.overrun
}
