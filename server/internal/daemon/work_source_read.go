package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/beads"
)

const (
	// workSourceReadDefaultLimit applies when a list request leaves Limit at 0.
	workSourceReadDefaultLimit = 50
	// workSourceReadMaxLimit caps list sizes so a hostile request cannot ask
	// for an unbounded scan.
	workSourceReadMaxLimit = 200
	// workSourceReadMaxOutputBytes bounds the marshaled JSON returned to the
	// caller, matching the bead client's output budget philosophy at a
	// slightly smaller cap.
	workSourceReadMaxOutputBytes = 2 << 20
)

// WorkSourceReadRequest is the minimal DTO the future polling integration
// will hand to executeWorkSourceRead. Command selects the read: "list" or
// "read". NativeID is required for "read" and rejected for "list"; Limit
// applies to "list" only (0 = default 50, capped at 200).
type WorkSourceReadRequest struct {
	WorkspaceID  string
	SourceHandle string
	Command      string
	NativeID     string
	Limit        int
}

// executeWorkSourceRead performs one bounded, read-only Beads source access
// by resolving the request against the operator-local bindings copied into
// Config at load time. The server never supplies paths or executables; a
// request whose (workspace, handle) has no local binding is rejected before
// any process is launched. Only "list" and "read" are supported; there is no
// write path here. The result is canonically re-marshaled typed output,
// bounded to 2 MiB.
func executeWorkSourceRead(ctx context.Context, bindings []cli.WorkSourceReadBinding, req WorkSourceReadRequest) (json.RawMessage, error) {
	switch req.Command {
	case "list":
		if req.NativeID != "" {
			return nil, fmt.Errorf("work source read: native_id is not valid for list")
		}
	case "read":
		if strings.TrimSpace(req.NativeID) == "" {
			return nil, fmt.Errorf("work source read: read requires native_id")
		}
		if req.Limit != 0 {
			return nil, fmt.Errorf("work source read: limit is not valid for read")
		}
	default:
		return nil, fmt.Errorf("work source read: unsupported command %q", req.Command)
	}
	limit := req.Limit
	if limit == 0 {
		limit = workSourceReadDefaultLimit
	}
	if limit < 0 || limit > workSourceReadMaxLimit {
		return nil, fmt.Errorf("work source read: limit must be between 0 and %d (got %d)", workSourceReadMaxLimit, req.Limit)
	}

	// Fail closed before any launch: the binding list itself must satisfy
	// the operator contract even if it bypassed the CLI parser (hand-edited
	// config file, programmatic save), so there is no duplicate-ambiguity and
	// no relative path can ever reach exec.
	if err := cli.ValidateWorkSourceReads(bindings); err != nil {
		return nil, fmt.Errorf("work source read: %w", err)
	}

	// Exclusively local binding resolution: exact workspace+handle match.
	// No fallback, no PATH lookup, no server-supplied values.
	var match *cli.WorkSourceReadBinding
	for i := range bindings {
		if bindings[i].WorkspaceID == req.WorkspaceID && bindings[i].SourceHandle == req.SourceHandle {
			match = &bindings[i]
			break
		}
	}
	if match == nil {
		return nil, fmt.Errorf("work source read: no local binding for workspace %s handle %q", req.WorkspaceID, req.SourceHandle)
	}

	client := &beads.Client{Executable: match.Executable, BeadsDir: match.BeadsDir}
	var payload any
	if req.Command == "list" {
		rows, err := client.List(ctx, limit)
		if err != nil {
			return nil, fmt.Errorf("work source read: %w", err)
		}
		// The beads parser bounds per-row shape but not row count; enforce
		// the requested limit here so a hostile source cannot smuggle an
		// oversized list past it.
		if len(rows) > limit {
			return nil, fmt.Errorf("work source read: list returned %d rows, exceeding limit %d", len(rows), limit)
		}
		payload = rows
	} else {
		issue, err := client.ReadTask(ctx, req.NativeID)
		if err != nil {
			return nil, fmt.Errorf("work source read: %w", err)
		}
		payload = issue
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("work source read: encode payload: %w", err)
	}
	if len(out) > workSourceReadMaxOutputBytes {
		return nil, fmt.Errorf("work source read: payload exceeds %d bytes", workSourceReadMaxOutputBytes)
	}
	return out, nil
}
