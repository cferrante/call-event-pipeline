# Plan: milestone 2 (M2)

Status: **agreed** · Spec: [design.md](design.md) · Previous: [M1 plan](plan-m1.md)

M2 goal ([spec goal 4](design.md#1-goals)): isolate AI inference so that a webhook spike shows up
as **queue lag**, not as slower or failing inference. Done when the spike test shows the model's
p95 latency flat (±10%) between idle and spike, while the edge and attribution behave as in the
[M1 baseline](benchmarks.md).

Same way of working as M1: one step at a time, test-first with deliberate breaks, one commit per
step, deviations and review outcomes recorded at the bottom.

## Decisions to agree before starting

### D1. What the model does

A stub, as the spec says: no real AI calls in M2, so it costs $0. It "classifies" each
interaction into an intent (e.g. `sales`, `support`, `unknown`) with a score, from the SMS body
or call details, using a deterministic rule so tests are predictable. Results go to a new
`insights` table, one row per interaction.

A real model (e.g. the Claude API) can replace the stub later behind the same interface; that
would cost money per call, so it's out of scope for M2.

### D2. The stub must behave like a real model under overload

This is what makes M2 a real test. If the stub always takes, say, 200 ms no matter how many
calls run at once, *any* consumer would show flat latency and the experiment would prove
nothing. Real inference servers have a fixed capacity: beyond it, requests queue inside the
server and latency climbs, then they time out.

So the stub has a **capacity** and a base latency: **4 concurrent calls and 200 ms ± jitter**,
agreed as starting values. Each call beyond capacity adds latency, and past a limit it fails like a timeout.
Unit tests pin this behaviour down before anything relies on it.

### D3. How the consumer protects the model: a bounded worker pool

- A separate durable consumer, `inference`, on the `LEADS` stream, with its own progress, so a
  slow model never holds back attribution (and vice versa). This is the **bulkhead** pattern:
  separate compartments, so a flood in one doesn't sink the other.
- **N workers, with N = the model's capacity**, and the consumer's **`MaxAckPending` = N**, a
  limit JetStream enforces on the server: it never hands out more than N unfinished messages,
  however many inference processes are running.
- During a spike, messages wait in the stream (lag) instead of piling onto the model. That's
  backpressure: the slow part sets the pace, and the queue absorbs the difference.

### D4. Priority lanes: in M2, or later?

The spec sketches "first-touch leads before re-analysis". Three options:

| Option | How | Trade-off |
|---|---|---|
| **A. Defer to M3** (recommended) | M2 is one pool, oldest first | Proves the core goal (flat model latency) with the fewest moving parts. Priority only matters once there's a backlog *and* some work is more urgent. |
| B. Prioritise inside the worker | Workers look up whether the caller is new and serve new callers first from their small pull batch | Little effect: the batch is 10 messages, so it can only reorder within those |
| C. Attribution publishes "first touch" / "repeat" events | Two inference consumers, each with its own pool | Real priority, but publishing after a database commit is the classic **dual-write** problem; doing it safely needs the **transactional outbox** pattern, a topic big enough for its own milestone |

**Decision: A.** Priority lanes, with option C and the outbox pattern, are the core of M3.

### D5. What to measure

Two different latencies, which are easy to confuse:

| Metric | Meaning | Expected under a spike |
|---|---|---|
| **Model latency** p50/p95 | Time inside the model call | **Flat**: the goal of M2 |
| **Time to insight** p50/p95 | From webhook received to insight stored | **Rises, then recovers**: that's the lag, by design |

Plus, as in M1: edge p99, zero loss/duplicates in `insights`, attribution drain time.

To prove the protection works, the spike test also runs a **control**: the same spike with the
pool limit removed (e.g. 200 workers). The model should then be overloaded: latency up, timeouts
and retries. If the control looks the same as the real run, the experiment is broken. This is
the deliberate break for the whole milestone.

## Steps

### Step 0: Agree this plan

On branch `docs/plan-m2`; merged before any code.

### Step 1: Update the spec

Replace the spec's [inference sketch](design.md#54-inference-consumer-cmdinference) with the
agreed design (D1–D5), add the `insights` table to the data model, and refine the inference
targets in [section 8](design.md#8-measuring-absorbing-spikes).
- Commit: *Specify the inference consumer*.

### Step 2: Stub model (`internal/inference`, unit)

- A `Model` interface (`Classify(ctx, input) (Result, error)`) and the stub.
- Tests first: deterministic intent for given inputs; base latency within capacity; latency
  rising beyond capacity; failure past the overload limit; respects context cancellation.
  Latency is driven by an injectable clock so the tests run in milliseconds.
- Commit: *Add a stub inference model with fixed capacity*.

### Step 3: Insights table and store (integration)

- Migration `0004`: `insights` (provider_id unique, intent, score, model_version, model_ms,
  received_at, created_at).
- Store: idempotent insert (`ON CONFLICT (provider_id) DO NOTHING`), as in attribution.
- Tests first: stored once; redelivery changes nothing; concurrent duplicates apply once.
- Commit: *Store inference results idempotently*.

### Step 4: Worker pool in the consumer package (integration)

- Generalise `internal/consumer` so `Run` can process with N workers, and `EnsureConsumer` takes
  per-consumer settings (name, `MaxAckPending`). Dead letters get a `Dlq-Consumer` header, since
  two consumers now share the DLQ.
- Tests first: never more than N messages in progress at once (checked from inside the
  recorder); `MaxAckPending` enforced by JetStream; attribution's behaviour unchanged (its
  existing tests stay green).
- Commit: *Process messages with a bounded worker pool*.

### Step 5: Inference service (`cmd/inference`) and end-to-end test

- Config from env (`DATABASE_URL`, `NATS_URL`, `INFERENCE_WORKERS`, stub settings), migrations at
  startup, graceful shutdown, as for attribution.
- e2e: a signed webhook produces both an attributed interaction and an insight; stopping the
  inference service doesn't slow ingest or attribution, and its backlog drains on restart.
- README: run instructions; click-through steps for you.
- Commit: *Wire up the inference service*.

### Step 6: Spike test with inference

- `run.sh` also starts the inference service and reports model latency and time to insight,
  each as p50/p95 for an idle window and the spike window, plus inference lag.
- Runs: the real configuration, and the **control** with the pool limit removed.
- Record both in [benchmarks](benchmarks.md) and the result here.
- Commit: *Add inference to the spike test and record results*.

### Step 7: Concepts guide

Add a "Life of an insight" walkthrough beside "Life of a lead", and new terms: bulkhead,
worker pool, `MaxAckPending`, capacity, control experiment. (Model latency, time to insight,
dual write and transactional outbox were added with this plan.)
- Commit: *Explain the inference concepts*.

### Step 8: Code review

`/code-review` on the M2 branch; fix or decline each finding and record outcomes below.

## Deviations from the plan

- **Step 4: dead-letter IDs now include the consumer's name.** A dead letter's de-duplication
  ID was built from the message's stream position alone, so once two consumers share the DLQ,
  the second consumer's dead letter for the same message was silently dropped as a duplicate.
  Found while planning the step's tests; a test showed only 1 of the expected 2 dead letters.
- **Step 4: the client may hold one more message than in M1**: up to the pull batch, plus one
  per worker, plus one waiting in the fetch loop to be handed to a worker. For attribution that's
  12, where M1's test allowed 11; still processable within `AckWait` (12 × 2s = 24s < 30s).
  Running M1's test 5 times caught this; a single run under the race detector had passed.
- **Step 4: the shutdown test now flushes the connection before checking acks.** Acks are sent
  without waiting for the server, so the check occasionally ran before the last ack arrived
  (1 failure in 10 runs under the race detector). The flush is a round trip, which guarantees
  earlier acks have arrived; 30 of 30 runs pass. Production already drains the connection
  before exiting (M1 code-review finding 5).
- **Step 5: shared settings moved to `internal/env`** (default NATS URL, the required
  `DATABASE_URL` check, password redaction). The inference service would have been the third
  copy of each; M1 declined this extraction (code-review finding 9) until a third copy appeared.
- **Step 6: the spike test uses a faster model profile: 10 ms latency, 200 ms timeout.** With the
  service defaults (200 ms, 2 s) the model handles about 20 messages/s, so a 70-second spike at
  1,000/s would take nearly an hour to drain. The profile is 20× faster with the same ratios
  (capacity 4, timeout = 20 × latency): about 400/s, still well below the spike, so a backlog
  still builds. The service defaults are unchanged.
- **Step 6: k6 starts with an idle period** (20/s for 20 s) as the baseline for "model latency
  flat between idle and spike".
- **Step 6: `MODE=control` judges the opposite outcome.** With 200 workers the model is
  expected to overload, so its check is "overload was visible" (timeouts, or spike p95 at least
  2× idle); losing work there is the point, so its inference loss and dead letters are reported,
  not judged. Short runs proved each check: normal checks with 200 workers fail on flatness
  (p95 11 → 190 ms, ~1,500 timeouts); control with 4 workers fails on "overloaded"; stopping
  inference fails the insight-loss and drain checks.
- **Step 7: "congestion collapse" was added to the concepts guide**, beyond the planned terms,
  because the control run demonstrated it.
- **Step 6 found an M1 bug: the ingest server closed idle connections after 10 seconds.** The
  first full run failed 2 of 71,372 requests. With k6 now logging failures, an idle-heavy run
  reproduced 7, all with no HTTP response (`EOF`, `connection reset by peer`): the keep-alive
  race, where a client sends a request on a connection the server is closing. `IdleTimeout` was
  never set, so Go used `ReadTimeout` (10 s). It's now 120 s, longer than Go clients' 90 s, so
  clients close idle connections first. Same run after the fix: 0 failures. Twilio would have
  hit this too, and had to retry.

## Code-review outcomes

Step 8 ran `/code-review` (medium) on the M2 branch: 9 findings, 6 fixed and 2 declined, plus
one decision (finding 1) made by you. Fixes were made test-first with deliberate breaks.

1. **Fixed (decision: option A): a new inference consumer read the whole 7-day history**, spending
   model capacity on stale leads at first start. It now starts at new messages; attribution still
   reads everything. JetStream can't change an existing consumer's start position, so starting
   against an old consumer fails with instructions, and `-reset-consumer` recreates it. Tests: old
   messages skipped (5 of 5 processed before the fix), attribution unaffected, the guidance error
   (before: a bare "deliver policy can not be updated"), and the reset flag end to end.
3. **Fixed: the spike test's drain wait trusted a NATS stats call whose failure was ignored**, and
   no longer checked row counts. Stats reads are now retried and, if they keep failing, report "?",
   which no check accepts; the drain waits for the row counts again; and an undrained consumer is
   reported as "did not drain" rather than with a misleading time. Break: the stats endpoint made
   unreachable during the drain gives a clean FAIL with no bash errors.
4. **Fixed: the e2e tests' inference start-up wait passed immediately**, because attribution had
   already created the tables. It now waits for the `inference` consumer and fails at once, with
   the process output, if inference exits. A new test starts inference alone on a fresh database
   (the step 5 gap); skipping its migrations is now caught. Made in the same commit as finding 1,
   because the restructured bulkhead test needed it.
2. **Fixed: model latency was stored in whole milliseconds**, so at the load test's 10 ms, 1 ms of
   rounding was as large as the 10% "flat" tolerance. It's now fractional (migration `0005`), and
   the spike test compares with decimals. Short runs: 4 workers, p95 11.71 ms idle vs 11.67 ms
   spike (PASS); 200 workers, 11.80 vs 190.25 ms (FAIL, as it should).
5. **Fixed: the spike test's idle and spike windows started at the earliest *insight*.** In control
   mode early messages may be dead-lettered, which shifted both windows. They now start at the
   earliest interaction. Simulated by deleting the first 2 s of insights: before, the idle window
   took 260 messages (mostly ramp-up) and the spike window 911; after, 30 and exactly 1,000.
6. **Fixed: `INFERENCE_WORKERS` defaulted to 4 regardless of `MODEL_CAPACITY`.** It now follows the
   capacity unless set.
9. **Fixed: workers and `MaxAckPending` were set separately for inference.** `consumer.Inference(n)`
   sets both, next to `consumer.Attribution`.

**Declined:**

7. **"During an upgrade, a dead letter could be stored twice"**, because its de-duplication ID now
   includes the consumer name. True only if an old and a new process handle the same failing
   message within the 2-minute de-duplication window, and there's no deployed system to upgrade.
8. **"Move the number and duration parsing helpers into `internal/env`."** There's one copy; as
   in M1, a shared helper waits for the third.

## Results

### Spike test

At `8fcfd76`, `./loadtest/run.sh` (normal) and `MODE=control ./loadtest/run.sh`: 20 s idle at 20/s,
then 1,000/s for 70 s, with the load-test model profile. **Both PASS.** Re-run at `b0c2515` after
the code-review fixes: both PASS again, with model p95 11.85 ms idle and 11.66 ms in the spike;
the control run drained much more slowly than before (see [benchmarks](benchmarks.md)). Details
and curves are in [benchmarks](benchmarks.md).

| Target ([spec section 8](design.md#8-measuring-absorbing-spikes)) | Result | |
|---|---|---|
| Model latency p95 flat (±10%) between idle and spike | 11 ms idle, 11 ms spike | ✓ |
| Time to insight rises during the spike, recovers after | p95 16 ms idle → 149 s spike; backlog drained 162 s after | ✓ |
| Inference loss/duplicates | 67,999 accepted, 67,999 insights; 0 dead letters | ✓ |
| Control run: model visibly overloaded | p95 12 → 190 ms, 29,610 timeouts, 10 dead letters | ✓ |
| Edge and attribution as in M1 | Edge: 0 errors, p99 3.6 ms. Attribution: zero loss, but slower to drain (154 s), since it shares Postgres and CPU with inference | ✓ with note |

The control run also showed the strongest argument for the worker pool: with 50 times the
workers, inference finished in twice the time, because retries of timed-out calls used up
model capacity (congestion collapse).
