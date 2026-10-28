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

Replace the spec's [inference sketch](design.md#54-inference-consumer-m2-sketch-only) with the
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

_None yet._

## Code-review outcomes

_None yet._

## Results

_Recorded in step 6._
