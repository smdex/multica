DO $$
DECLARE
    has_native_sources BOOLEAN;
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = to_regclass('work_source')
          AND attname = 'native_request_id' AND NOT attisdropped
    ) THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM work_source WHERE native_request_id IS NOT NULL)'
            INTO has_native_sources;
        IF has_native_sources THEN
            RAISE EXCEPTION 'remove native enrollment sources before reverting their ownership provenance';
        END IF;
    END IF;
END $$;

ALTER TABLE IF EXISTS work_source
    DROP COLUMN IF EXISTS native_request_id,
    DROP COLUMN IF EXISTS native_request_hash,
    DROP COLUMN IF EXISTS native_enrollment_id,
    DROP COLUMN IF EXISTS native_owner_member_id,
    DROP COLUMN IF EXISTS native_runtime_created_at,
    DROP COLUMN IF EXISTS native_manifest_hash,
    DROP COLUMN IF EXISTS native_approved_at,
    DROP COLUMN IF EXISTS native_enrolled_at;
