-- Email leads (schema 1.1); see docs/design.md, sections 5.1 and 7.
--
-- Additive only: running attribution and inference binaries from before this migration keep
-- working, because no column they use is renamed or removed. from_number keeps its name and
-- holds the sender's address for email: a rename would break running instances mid-deploy
-- and need an expand-and-contract migration for a cosmetic gain.

ALTER TABLE interactions DROP CONSTRAINT interactions_channel_check;
ALTER TABLE interactions ADD CONSTRAINT interactions_channel_check
    CHECK (channel IN ('voice', 'sms', 'email'));

-- An email is sent to a tracking address, not a tracking number.
ALTER TABLE interactions ALTER COLUMN to_e164 DROP NOT NULL;
ALTER TABLE interactions ADD COLUMN to_address text;
ALTER TABLE interactions ADD CONSTRAINT interactions_one_destination
    CHECK ((to_e164 IS NULL) <> (to_address IS NULL));

-- Which campaign each tracking address belongs to: the email equivalent of tracking_numbers.
-- Stored lowercased, so lookups (which lowercase too) can't miss on case.
CREATE TABLE tracking_addresses (
    address     text PRIMARY KEY CHECK (address = lower(address)),
    campaign_id text NOT NULL,
    source      text NOT NULL,
    medium      text NOT NULL DEFAULT '',
    active      boolean NOT NULL DEFAULT true
);
