-- AI results, one per interaction; see docs/design.md, sections 5.4 and 7.
--
-- No foreign key to interactions: the inference and attribution consumers run at their own
-- pace, so an insight can be stored before its interaction. They join on provider_id.
CREATE TABLE insights (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id   text NOT NULL UNIQUE,
    intent        text NOT NULL CHECK (intent IN ('sales', 'support', 'unknown')),
    score         real NOT NULL CHECK (score BETWEEN 0 AND 1),
    model_version text NOT NULL,
    model_ms      integer NOT NULL CHECK (model_ms >= 0),    -- model latency
    received_at   timestamptz NOT NULL,                      -- when the webhook arrived
    created_at    timestamptz NOT NULL DEFAULT now()         -- created_at − received_at = time to insight
);
