-- Initial schema; see docs/design.md, section 7.

-- Which campaign each tracking number belongs to. Looked up on every interaction.
CREATE TABLE tracking_numbers (
    phone_e164  text PRIMARY KEY,
    campaign_id text NOT NULL,
    source      text NOT NULL,
    medium      text NOT NULL DEFAULT '',
    active      boolean NOT NULL DEFAULT true
);

-- One row per identifiable caller, with precomputed first and last touch.
-- campaign_id is NULL when the touch came through an unknown tracking number.
CREATE TABLE leads (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    phone_e164        text NOT NULL UNIQUE,
    first_campaign_id text,
    first_source      text NOT NULL,
    first_seen_at     timestamptz NOT NULL,
    last_campaign_id  text,
    last_source       text NOT NULL,
    last_seen_at      timestamptz NOT NULL
);

-- The record of facts: one row per inbound call or SMS, never rewritten.
-- campaign_id and source are what the tracking number meant when the interaction happened.
CREATE TABLE interactions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id text NOT NULL UNIQUE,
    lead_id     bigint REFERENCES leads (id),            -- NULL for anonymous callers
    channel     text NOT NULL CHECK (channel IN ('voice', 'sms')),
    from_number text NOT NULL,                           -- may be "anonymous", not E.164
    to_e164     text NOT NULL,
    campaign_id text,                                    -- NULL for unknown tracking numbers
    source      text NOT NULL,
    occurred_at timestamptz NOT NULL,
    ingested_at timestamptz NOT NULL DEFAULT now(),
    raw         jsonb NOT NULL
);

-- Postgres doesn't index foreign keys on its own.
CREATE INDEX interactions_lead_id_idx ON interactions (lead_id);
