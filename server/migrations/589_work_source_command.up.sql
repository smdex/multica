CREATE TABLE IF NOT EXISTS work_source_command (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    source_id UUID NOT NULL,
    -- Stable client UUID identifies retries through terminal completion.
    request_id UUID NOT NULL,
    config_revision INTEGER NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '5 minutes'),
    -- Read-only allowlist per the T01 qualification: bd create/update lack
    -- a revision precondition and atomic attribution, so writes stay out.
    command TEXT NOT NULL CHECK (command IN ('list', 'read')),
    -- Opaque source-native ID; required for 'read', NULL for 'list'.
    native_id TEXT,
    -- Requested list size, 1..200 enforced in the service; NULL for 'read'.
    limit_count INTEGER,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'claimed', 'succeeded', 'failed')),
    -- Runtime that claimed the command; owner-routing is enforced by
    -- comparing the source's daemon_id with the runtime's daemon identity.
    claimed_runtime_id UUID,
    claimed_at TIMESTAMPTZ,
    -- sha256(command | '\x1f' | native_id | '\x1f' | limit): detects a
    -- changed payload for the same request_id, including terminal retries.
    request_hash TEXT NOT NULL,
    -- Bounded command result (JSON for 'list'/'read' payloads); set only
    -- on succeeded.
    result TEXT,
    error TEXT,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
