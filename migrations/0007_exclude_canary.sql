-- The monitor sends a synthetic "canary" webhook every 10 seconds (From = 'canary') to prove the
-- whole path works; see docs/design.md, section 8.1. It is not a lead, so the campaign report
-- leaves it out. Same view as 0003 otherwise.

CREATE OR REPLACE VIEW campaign_conversations AS
WITH gaps AS (
    SELECT campaign_id,
           source,
           lead_id,
           occurred_at - lag(occurred_at) OVER (
               PARTITION BY from_number, to_e164, campaign_id ORDER BY occurred_at
           ) AS gap -- NULL for a caller's first interaction with this number and campaign
    FROM interactions
    WHERE from_number <> 'canary'
)
SELECT campaign_id,
       source,
       count(*) FILTER (
           WHERE lead_id IS NULL OR gap IS NULL OR gap > interval '24 hours'
       )        AS conversations,
       count(*) AS interactions
FROM gaps
GROUP BY campaign_id, source;
