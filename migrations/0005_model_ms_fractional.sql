-- Store model latency with sub-millisecond precision (found by code review).
--
-- At the load test's 10 ms model latency, whole milliseconds make 1 ms of rounding as large as
-- the 10% tolerance for "model latency stays flat", so the check could pass or fail on rounding.
ALTER TABLE insights ALTER COLUMN model_ms TYPE double precision;
