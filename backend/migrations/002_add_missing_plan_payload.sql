BEGIN;

ALTER TABLE plans ADD COLUMN IF NOT EXISTS payload JSONB;

CREATE INDEX IF NOT EXISTS idx_plans_payload_gin
    ON plans USING GIN (payload);

COMMIT;
