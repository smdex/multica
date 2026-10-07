ALTER TABLE work_source
    ADD COLUMN IF NOT EXISTS native_request_id UUID,
    ADD COLUMN IF NOT EXISTS native_request_hash TEXT,
    ADD COLUMN IF NOT EXISTS native_enrollment_id UUID,
    ADD COLUMN IF NOT EXISTS native_owner_member_id UUID,
    ADD COLUMN IF NOT EXISTS native_runtime_created_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS native_manifest_hash TEXT,
    ADD COLUMN IF NOT EXISTS native_approved_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS native_enrolled_at TIMESTAMPTZ;
