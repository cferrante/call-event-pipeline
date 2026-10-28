-- Conversations per campaign; see docs/design.md, section 7.
--
-- A conversation is a run of interactions from one caller to one tracking number (any
-- channel), with no gap longer than 24 hours. Reports count conversations, so a chatty SMS
-- thread is one lead contact, not many. Anonymous callers can't be told apart, so each of
-- their interactions is its own conversation.

-- Lets the window function read each caller/number pair already in time order.
CREATE INDEX interactions_conversation_idx ON interactions (from_number, to_e164, occurred_at);

CREATE VIEW campaign_conversations AS
WITH gaps AS (
    SELECT campaign_id,
           source,
           lead_id,
           occurred_at - lag(occurred_at) OVER (
               PARTITION BY from_number, to_e164 ORDER BY occurred_at
           ) AS gap -- NULL for a caller's first interaction with this number
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
