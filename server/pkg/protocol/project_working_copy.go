package protocol

import "encoding/json"

// ProjectWorkingCopySpec is the durable daemon-side provisioning instruction.
// Path fields are inputs only; a daemon reports CanonicalPath after it has
// validated or materialized the target filesystem object.
type ProjectWorkingCopySpec struct {
	LocalPath  string `json:"local_path,omitempty"`
	RepoURL    string `json:"repo_url,omitempty"`
	ParentPath string `json:"parent_path,omitempty"`
	Branch     string `json:"branch,omitempty"`
	BaseRef    string `json:"base_ref,omitempty"`
}

// ProjectWorkingCopy is shared by the authenticated API, daemon reconciliation,
// and task claim payload. It deliberately carries no tenant Workspace object;
// its workspace_id and project_id are the authoritative ownership boundary.
type ProjectWorkingCopy struct {
	ID                    string          `json:"id"`
	WorkspaceID           string          `json:"workspace_id"`
	ProjectID             string          `json:"project_id"`
	DaemonID              string          `json:"daemon_id"`
	Name                  string          `json:"name"`
	Kind                  string          `json:"kind"`
	CanonicalPath         string          `json:"canonical_path,omitempty"`
	Ownership             string          `json:"ownership"`
	SourceResourceID      string          `json:"source_resource_id,omitempty"`
	State                 string          `json:"state"`
	ProvisioningRequestID string          `json:"provisioning_request_id"`
	ProvisioningSpec      json.RawMessage `json:"provisioning_spec"`
	LastError             string          `json:"last_error,omitempty"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	ArchivedAt            string          `json:"archived_at,omitempty"`
	DeleteRequestedAt     string          `json:"delete_requested_at,omitempty"`
}
