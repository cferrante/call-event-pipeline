-- Split conversations by campaign as well (fixes 0002; found by code review).
--
-- A tracking number can be moved to another campaign while a caller is mid-conversation. With
-- conversations split only by caller and number, the new campaign got the later interactions
-- but none of the conversation. Now each campaign counts its own part of it.

DROP INDEX interactions_conversation_idx;
CREATE INDEX interactions_conversation_idx
    ON interactions (from_number, to_e164, campaign_id, occurred_at);

CREATE OR REPLACE VIEW campaign_conversations AS
WITH gaps AS (
    SELECT campaign_id,
           source,
           lead_id,
           occurred_at - lag(occurred_at) OVER (
               PARTITION BY from_number, to_e164, campaign_id ORDER BY occurred_at
           ) AS gap -- NULL for a caller's first interaction with this number and campaign
    FROM interactions
)
SELECT campaign_id,
       source,
       count(*) FILTER (
           WHERE lead_id IS NULL OR gap IS NULL OR gap > interval '24 hours'
       )        AS conversations,
       count(*) AS interactions
FROM gaps
GROUP BY campaign_id, source;
