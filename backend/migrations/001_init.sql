BEGIN;

CREATE TABLE IF NOT EXISTS plans (
    id VARCHAR(32) PRIMARY KEY,
    status VARCHAR(20) NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    share_url TEXT,
    title TEXT,
    summary TEXT,
    closing_note TEXT,
    vibe VARCHAR(64),
    start_at TIMESTAMPTZ,
    end_at TIMESTAMPTZ,
    total_walk_minutes INTEGER,
    total_walk_meters INTEGER,
    feasibility VARCHAR(32),
    dining_cost_jpy INTEGER DEFAULT 0,
    lodging_cost_jpy INTEGER DEFAULT 0,
    transit_cost_jpy INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS plan_segments (
    id VARCHAR(64) PRIMARY KEY,
    plan_id VARCHAR(32) NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    segment_order INTEGER NOT NULL,
    segment_type VARCHAR(20) NOT NULL,
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    narrative_headline TEXT,
    narrative_reason TEXT,
    narrative_tip TEXT,
    buffer_kind VARCHAR(32),
    buffer_at_place_name TEXT,
    move_distance_meters INTEGER,
    move_duration_minutes INTEGER,
    move_from_name TEXT,
    move_to_name TEXT,
    move_maps_url TEXT,
    dining_candidate_id TEXT,
    dining_name TEXT,
    dining_rating NUMERIC(3,2),
    dining_user_rating_count INTEGER,
    dining_price_level INTEGER,
    dining_estimated_cost_per_person_jpy INTEGER,
    dining_address TEXT,
    dining_photo_url TEXT,
    dining_phone_number TEXT,
    dining_open_now BOOLEAN,
    dining_closes_at TIMESTAMPTZ,
    lodging_candidate_id TEXT,
    lodging_name TEXT,
    lodging_review_average NUMERIC(3,2),
    lodging_review_count INTEGER,
    lodging_address TEXT,
    lodging_photo_url TEXT,
    lodging_plan_name TEXT,
    lodging_room_name TEXT,
    lodging_total_price_jpy INTEGER,
    lodging_price_per_person_jpy INTEGER,
    lodging_vacancy_status VARCHAR(20),
    lodging_remaining_rooms INTEGER,
    lodging_check_in_time TIME,
    lodging_check_in_deadline TIME,
    lodging_check_out_time TIME,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(plan_id, segment_order)
);

CREATE TABLE IF NOT EXISTS plan_events (
    id BIGSERIAL PRIMARY KEY,
    plan_id VARCHAR(32) NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    event_type VARCHAR(20) NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status VARCHAR(20),
    provider VARCHAR(32),
    source_state VARCHAR(20),
    source_latency_ms INTEGER,
    cached BOOLEAN,
    result_count INTEGER,
    error_code VARCHAR(64),
    error_message TEXT,
    payload JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS click_events (
    id BIGSERIAL PRIMARY KEY,
    plan_id VARCHAR(32) NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    segment_id VARCHAR(64),
    tracking_id TEXT NOT NULL,
    provider VARCHAR(32) NOT NULL,
    clicked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    request_id VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_plans_status ON plans(status);
CREATE INDEX IF NOT EXISTS idx_plans_expires_at ON plans(expires_at);
CREATE INDEX IF NOT EXISTS idx_plan_segments_plan_id ON plan_segments(plan_id);
CREATE INDEX IF NOT EXISTS idx_plan_events_plan_id ON plan_events(plan_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_click_events_plan_id ON click_events(plan_id, clicked_at DESC);
CREATE INDEX IF NOT EXISTS idx_click_events_tracking_id ON click_events(tracking_id);

CREATE OR REPLACE FUNCTION touch_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_plans_touch_updated_at
BEFORE UPDATE ON plans
FOR EACH ROW
EXECUTE FUNCTION touch_updated_at();

COMMIT;
