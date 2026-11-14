# Benchmarks

Spike-test results over time, so the effect of a change can be measured rather than guessed.
Run with `./loadtest/run.sh` (see the [README](../README.md#running-locally)); it ends by printing
a row for this table. Add a row for baselines and for runs that test a change, and say what
changed. Raw output (logs, `lag.csv`) stays in the git-ignored `loadtest/results/`.

Numbers are only comparable between rows with the same machine.

## Results

| Date | Commit | Machine | Load | Edge p50 / p99 | Errors | Peak lag | Drained after | Rows | Dead letters |
|---|---|---|---|---|---|---|---|---|---|
| 2026-10-28 | `2f7278d` | 8 CPU, Ubuntu WSL2 | 1000/s, ramp 10s, hold 60s | 0.42 / 1.03 ms | 0.000% | 22675 | 27s | ✓ 67499 | 0 |
| 2026-10-28 | `607534e` | 8 CPU, Ubuntu WSL2 | 1000/s, ramp 10s, hold 60s | 0.47 / 2.02 ms | 0.000% | 27123 | 37s | ✓ 67499 | 0 |
| 2026-10-28 | `8fcfd76` | 8 CPU, Ubuntu WSL2 | 1000/s, ramp 10s, hold 60s; + inference (normal) | 0.71 / 3.60 ms | 0.000% | 44105 | 154s | ✓ 67999 | 0 |
| 2026-10-28 | `8fcfd76` | 8 CPU, Ubuntu WSL2 | 1000/s, ramp 10s, hold 60s; + inference (control) | 0.71 / 3.66 ms | 0.000% | 41741 | 90s | ✓ 67999 | 10 (inference) |
| 2026-10-28 | `b0c2515` | 8 CPU, Ubuntu WSL2 | 1000/s, ramp 10s, hold 60s; + inference (normal) | 0.67 / 3.04 ms | 0.000% | 40749 | 142s | ✓ 67999 | 0 |
| 2026-10-28 | `b0c2515` | 8 CPU, Ubuntu WSL2 | 1000/s, ramp 10s, hold 60s; + inference (control) | 0.71 / 3.17 ms | 0.000% | 41671 | 682s | ✓ 67999 | 0 |
| 2026-11-14 | `597128a`+fix | AWS c7g.xlarge (k6 on c7g.large) | 1000/s, ramp 10s, hold 30s; + inference (demo spike) | 0.45 / 0.96 ms | 0.000% | ~14600 | 69s | ✓ 37799 | 0 |
| 2026-11-14 | `693c618` | AWS c7g.xlarge (k6 on c7g.large) | 1000/s, ramp 10s, hold 60s; + inference (normal) | 0.47 / 1.05 ms | 0.000% | ~33177 | 131s | ✓ 67799 | 0 |
| 2026-11-14 | `693c618`+fix | AWS c7g.xlarge (k6 on c7g.large) | 1000/s, ramp 10s, hold 60s; + inference (normal) | 0.47 / 0.99 ms | 0.000% | ~30511 | 133s | ✓ 67799 | 0 |
| 2026-11-14 | `693c618`+fix | AWS c7g.xlarge (k6 on c7g.large) | 1000/s, ramp 10s, hold 60s; + inference (normal) | 0.46 / 0.96 ms | 0.000% | not recorded | 135s | ✓ 67799 | 0 |
| 2026-11-14 | `693c618`+fix | AWS c7g.xlarge (k6 on c7g.large) | 1000/s, ramp 10s, hold 60s; + inference (control) | 0.46 / 0.98 ms | 0.000% | ~30700 | 374s | ✓ 67799 | 1 |

The AWS rows are `make demo-spike` (`MODE=control` for the control row); the first is the demo
path's 30 s hold, the rest the standard 60 s. Their peak lag is sampled every few seconds, so
approximate (one run's was lost to a filter on the output), and "drained" covers both consumers.
`+fix` is the exit-status fix to `spike.sh`, which changes nothing the run measures. **The three
normal runs agree within 5%** (edge p99 0.96–1.05 ms, drained 131–135 s), unlike the laptop's
20–25%; the server is a steadier place to compare changes. See [server benchmarks](#server-benchmarks-m3).

Columns: **Load** is peak rate, ramp and hold. **Edge** latency is k6's view of the ingest
service. **Peak lag** is the attribution consumer's largest backlog, and **Drained after** is how
long after the spike ended it reached 0. **Rows** is interactions in Postgres, ✓ when it's at
least the unique webhooks that got a 200 (zero loss) and at most the unique webhooks sent (zero
duplicates).

## Inference (M2)

| Date | Commit | Mode | Model p95 idle / spike | Time to insight p95 idle / spike | Peak inference lag | Drained after | Insights | Model timeouts | Dead letters |
|---|---|---|---|---|---|---|---|---|---|
| 2026-10-28 | `8fcfd76` | normal, 4 workers | 11 / 11 ms | 16 ms / 149 s | 45642 | 162s | 67999 (all) | 0 | 0 |
| 2026-10-28 | `8fcfd76` | control, 200 workers | 12 / 190 ms | 17 ms / 287 s | 47503 | 321s | 67989 (10 lost) | 29610 | 10 |
| 2026-10-28 | `b0c2515` | normal, 4 workers | 11.85 / 11.66 ms | 17 ms / 148 s | 45402 | 163s | 67999 (all) | 0 | 0 |
| 2026-10-28 | `b0c2515` | control, 200 workers | 11.84 / 189.76 ms | 16 ms / 797 s | 51754 | 837s | 67999 (all, after retries) | 25134 | 0 |
| 2026-11-14 | `693c618` | normal, 4 workers, AWS | 11.15 / 11.57 ms | < 0.1 s / 127 s | ~44438 | 131s | 67799 (all) | 0 | 0 |
| 2026-11-14 | `693c618`+fix | normal, 4 workers, AWS | 11.15 / 11.57 ms | < 0.1 s / 126 s | ~44197 | 133s | 67799 (all) | 0 | 0 |
| 2026-11-14 | `693c618`+fix | normal, 4 workers, AWS | 11.15 / 11.58 ms | < 0.1 s / 126 s | ~44164 | 135s | 67799 (all) | 0 | 0 |
| 2026-11-14 | `693c618`+fix | control, 200 workers, AWS | 11.15 / 190.25 ms | < 0.1 s / 325 s | ~48960 | 374s | 67798 (1 dead-lettered) | 32686 | 1 |

Rows come from the same runs as the table above (`run.sh` prints both). **Mode** is `normal`
(workers = model capacity) or `control` (the limit removed). The load-test model profile is
10 ms latency ±10%, 200 ms timeout, capacity 4 (see the [M2 plan](plan-m2.md#deviations-from-the-plan)):
31 times faster than the calibrated real model, so a spike drains in minutes. The stub's own
default is the calibrated profile, 309 ms ±75% ([model calibration](#model-calibration-m3)).
Model latency should stay flat in `normal`; time to insight rises during the spike by design.

## Crash test (M3)

| Date | Commit | Machine | Run | Kills | Webhooks | Sender retries | Drained after | Data | Dead letters |
|---|---|---|---|---|---|---|---|---|---|
| 2026-11-13 | `3d15a5c`+crash scripts | 8 CPU, Ubuntu WSL2 | crash, 100/s for 240s, seed 1 | 23 | 24001 | 170 | 6s | ✓ 24001 | 0 |
| 2026-11-13 | `3d15a5c`+crash scripts | 8 CPU, Ubuntu WSL2 | clean, 100/s for 240s, seed 1 | 0 | 24001 | 0 | 5s | ✓ 24001 | 0 |
| 2026-11-14 | `693c618` | AWS c7g.xlarge | crash, 100/s for 240s, seed 1 | 23 | 24001 | 175 | 5s | ✓ 24001 | 0 |
| 2026-11-14 | `1cdf79b` | AWS c7g.xlarge | clean, 100/s for 240s, seed 1 | 0 | 24001 | 0 | 6s | ✓ 24001 | 0 |

Run with `./loadtest/crash.sh` (`KILLS=0` for the clean run). **Kills** are `kill -9` of a random
service every 5–15 s; this run killed attribution 8 times, inference and `nats-server` 6 each,
and ingest 3. **Data** is ✓ when every interaction, insight and lead matches what the webhooks
say must exist, computed in SQL from the iteration numbers, with an identical checksum; both
runs gave `607d7f28…`. **Drained after** includes 5 s of confirming the counts hold.

**Deliberate break:** with attribution acknowledging *before* committing, a 60-second run at
100/s (seed 3, 7 kills) lost 1 interaction and the run failed. The commit-then-acknowledge order
is what the passing runs prove.

## Model calibration (M3)

A real hosted model, measured through Groq's free tier from the development machine (8 CPU,
Ubuntu WSL2, home broadband) on 2026-11-13, with `cmd/calibrate` and only synthetic texts. Run
it with the key read from SSM into the process's environment, never onto disk or a command line:

```bash
GROQ_API_KEY=$(aws ssm get-parameter --name /call-event-pipeline/groq-api-key --with-decryption \
  --query Parameter.Value --output text --profile <PROJECT_PROFILE>) go run ./cmd/calibrate
```

**Accuracy** on the 48 labelled texts in `cmd/calibrate/labelled.json` (16 per intent, SMS and
email; labels written before any model ran):

| Classifier | Accuracy | sales | support | unknown | Tokens a call |
|---|---|---|---|---|---|
| `openai/gpt-oss-20b`, reasoning effort low | 98% (47/48) | 16/16 | 16/16 | 15/16 | 257 |
| `qwen/qwen3.8-27b` | 100% (48/48) | 16/16 | 16/16 | 16/16 | 156 |
| stub keyword rules | 54% (26/48) | 6/16 | 6/16 | 14/16 | – |

`gpt-oss-20b`'s one miss, "How do I stop getting these texts?", is the most debatable label in the
set (an opt-out, labelled unknown; the model said support). One sample in 48 doesn't separate
the two models.

**Latency**, 100 calls one at a time at 20 a minute, through the limiter, timed by the client
(so it includes the home network):

| Model | Errors | 429s | min | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|
| `openai/gpt-oss-20b` | 0 | 0 | 107 ms | 309 ms | 519 ms | 825 ms | 849 ms |
| `qwen/qwen3.8-27b` | 0 | 0 | 149 ms | 273 ms | 594 ms | 895 ms | 1369 ms |

With 100 samples, p99 is the second-slowest call; treat it as indicative.

**Where 429s start** (`gpt-oss-20b`, limiter off, no retries, calls sent on schedule, a minute
apart between steps so each starts from an empty window):

| Calls a minute | Sent | OK | 429s | Tokens | First 429 after |
|---|---|---|---|---|---|
| 20 | 20 | 20 | 0 | 5154 | – |
| 30 | 30 | 30 | 0 | 7701 | – |
| 40 | 40 | 35 | 5 | 8959 | 45 s |
| 50 | 50 | 40 | 10 | 10237 | 36 s |

The limit is tokens, not requests: 8,000 tokens a minute at 257 tokens a call predicts about 31
calls a minute, and refusals begin between 30 and 40. With the limiter on, the latency run's 100
calls had no 429s. **So the free tier classifies about 30 leads a minute** (about 50 with Qwen's
smaller prompts), and 1,000 requests a day; a production deployment would need a paid tier.

**Model choice:** `openai/gpt-oss-20b` stays the default. Groq lists it as a production model;
`qwen3.8-27b` is a preview model ("may be discontinued at short notice") and, on the paid tier,
costs about ten times as much per token ($0.80 / $4.00 against $0.075 / $0.30 per million input
/ output tokens, Groq's models page, 2026-11-13). Qwen's advantage is only the free tier's
throughput.

The whole calibration spent about 93,000 tokens and cost $0.

## Server benchmarks (M3)

Measured on 2026-11-14 in the [demo environment](../infra/demo/README.md): the system under
test on a c7g.xlarge (4 Graviton vCPU, 8 GB, gp3 disk, Ubuntu 26.04) running NATS, Postgres and
the services under systemd; k6 on a c7g.large (2 vCPU) in the same availability zone. Code at
`693c618`, plus the script fixes in `2ce2442` and `1cdf79b`, which change no service. One
session, 74 minutes of instance time, about **$0.28** at list prices. Spike and crash rows are
in the tables above; the rest is here.

| Measurement | Target | Result |
|---|---|---|
| 1,000/s spike, normal (×3) | 0 lost, 0 duplicated, model p95 flat ±10% | ✓ all three: edge p99 0.96–1.05 ms, model p95 11.15 → 11.58 ms (+4%), drained in 131–135 s |
| 1,000/s spike, control | the model overloads without the limit | ✓ model p95 11.15 → 190 ms (17×), 32,686 timeouts, 1 dead letter; drained in 374 s against 133 s |
| Edge step-up | highest rate with p99 < 50 ms, 0 errors, load generator < 80% CPU | **at least 4,000/s** (p99 6.4 ms); above it the load generator, not the server, is the limit (below) |
| Attribution sustained throughput | measured, bottleneck named | **~620 messages/s**; one Postgres transaction per message, waiting on the commit (below) |
| CPU profiles | our code against I/O | **our code is 0.1% (ingest) and 0.5% (attribution) of their CPU**; each uses about 0.2 of a core at 1,000/s |
| Crash test | identical counts and checksum after ≥ 20 kills | ✓ 23 kills, 24,001 webhooks, checksum `607d7f28…`, the same as the clean run and the laptop's |
| Monitor | detects within 60 s; 0 alerts in normal spikes | ✓ 5.0 s to `[FIRING] acked_not_written`; 0 emails across the three normal spikes. The control run had false alerts (below) |
| Replay recovery | rows after replay equal the clean run | ✓ 625 lost, all 7,649 restored in 2.4 s; a second replay recorded 0 |
| `sync_interval: always` | edge p99 against the default | **caps the edge at 300–500/s**: p99 11.4 ms at 300/s (default ~1 ms); at 500/s and 1,000/s publishes time out (below) |

**Edge step-up.** 60 s per step, consumers and monitor paused (`make demo-stepup`):

| Rate/s | p50 ms | p99 ms | Dropped by k6 | Load generator CPU | Server CPU |
|---|---|---|---|---|---|
| 500 | 0.42 | 0.52 | 0 | 7% | 4% |
| 1,000 | 0.40 | 0.84 | 0 | 8% | 7% |
| 2,000 | 0.39 | 0.97 | 0 | 27% | 17% |
| 3,000 | 0.40 | 1.55 | 0 | 39% | 23% |
| 4,000 | 0.40 | 6.39 | 0 | 48% | 31% |
| 5,000 | 0.44 | 15.08 | 16 | 51% | 32% |
| 6,000 | 0.46 | 38.27 | 261 | 62% | 41% |
| 8,000 | 0.77 | 81.91 | 14,523 | 72% | 50% |

No request failed at any step. From 5,000/s k6 dropped iterations, so those steps fail the
criterion; the server's CPU stayed at or below 50%. A re-run of the top steps with 1,500 virtual
users started up front removed the drops at 5,000/s but made p99 *worse* (4,000/s: 23.6 ms;
5,000/s: 92.7 ms) as k6's own CPU rose to 60%. Latency that worsens when k6 does more work,
with the server two-thirds idle, says the 2-vCPU load generator sets the limit above 4,000/s.
CPU is the mean over both cores, so the 80% guard didn't trip. Finding the server's real
ceiling needs a bigger load generator.

**Attribution throughput.** 102,999 webhooks queued with the consumers stopped, then attribution
started alone (`make demo-throughput`): drained in 171 s at a steady 610–650/s. Attribution used
21% of one core and Postgres 32% of one; the machine was 15% busy with 14% iowait. In
attribution's profile, 60% of its CPU is under `Store.Record`'s transaction, and its own code
0.5%. **Nothing is saturated: each message waits for its own transaction's round trips and
commit, one at a time** (about 1.6 ms each). Batching messages into one transaction, or
handling several at once, is the M4 candidate ([D1](plan-m3.md#decisions-agreed-before-starting));
a faster language would not move this. It matches the laptop's ~625/s.

**CPU profiles.** 30 s during a normal spike's hold (`PROFILE=1`), each sample's leaf function
grouped:

| | ingest | attribution (spike) | attribution (drain) |
|---|---|---|---|
| CPU used | 24% of one core | 21% | 19% |
| Go runtime: scheduler, GC, memory | 50% | 43% | 46% |
| Syscalls: network and disk I/O | 27% | 32% | 32% |
| Standard library: HTTP, crypto, JSON | 18% | 15% | 14% |
| Libraries: NATS, pgx | 4% | 9% | 7% |
| **Our code** | **0.1%** | **0.5%** | **0.5%** |

Most of the runtime share is the scheduler parking and waking goroutines between I/O
(`futex`, `findRunnable`). Rewriting our code in another language would at best remove the last
row; the [decision record](decisions/0001-event-stream.md) uses this as its evidence.

**Monitor.** The drill (`make demo-fault`) ran 50/s through attribution's `faultinject` build and
turned on the silent write failure 30 s in; with the default 1-minute window the alert fired at
the next 5 s reading, 5.0 s after the fault, and resolved 55 s after it was turned off.
**False alerts in the control run:** `stale_backlog (inference)` fired and resolved, which is
right, as the model was overloaded. `acked_not_written (inference)` fired four times, which is
wrong: only one message was dead-lettered. The rule counts acknowledgements as the movement of
the consumer's ack floor, the oldest unacknowledged message. A message stuck in timeout retries
holds the floor while later ones are written; when it clears, the floor jumps past thousands in
one window and they look unwritten. Any heavy retrying could do the same, a hosted model's `429`
retries included.

**`sync_interval: always`.** NATS reported `sync_always: true` (default: `sync_interval` 120 s).
Each webhook then waits for its write to reach the disk:

| Rate/s, hold | Edge p50 / p99 | Failed | Stored |
|---|---|---|---|
| 300, 30 s | 3.30 / 11.44 ms | 0% | all |
| 500, 30 s | 2,884 / 5,001 ms | 18% | all, including the failed ones |
| 1,000, 60 s | 5,001 / 5,001 ms | 84% | all, including the failed ones |

Above about 300–500/s the syncs queue up and publishes reach ingest's 5 s deadline, so ingest
answers with an error and the sender retries. Nothing was lost: every accepted webhook was
stored once, and so was every failed one, because the stream finished storing them after ingest
stopped waiting (a sender's retry is then deduplicated). The default (sync every 2 minutes) risks up to
2 minutes of acknowledged webhooks on a power loss of the server; `always` closes that at a
tenth of the throughput. The trade-off is argued in the decision record.

## Notes

### 2026-10-28, `2f7278d`: M1 baseline

Attribution consumer processing one message at a time. Consumer lag, sampled every second:

```
   0s      0
  10s    401
  20s   3780 ######
  30s   7210 ############
  50s  14865 ########################
  60s  18854 ###############################
  70s  22531 #####################################   spike ends (peak 22,675 at 71s)
  80s  16799 ###########################
  90s   9018 ###############
 100s    233
 101s      0                                         drained
```

During the hold, lag grew by about 375 messages/s while 1,000/s arrived, so the consumer
processed roughly 625/s; after the spike it drained at about 750/s. The edge stayed near 1 ms
throughout because it only waits for JetStream.

**Run-to-run variance.** An earlier run of the same scripts (on uncommitted code, so not in
the table) peaked at 28,134 messages and drained in 37s. Expect differences of about 20–25%
between identical runs on a laptop; compare several runs before concluding a change helped.

### 2026-10-28, `607534e`: client pulls 10 messages at a time

Change: the attribution consumer pulls at most 10 messages per batch instead of up to 500
(code-review finding 3), so buffered messages can't expire before they're handled.

Peak lag 27,123 and drain 37s, against the baseline's 22,675 and 27s. That's within the
run-to-run variance noted above: an earlier run of the old code peaked at 28,134 and also
drained in 37s. **No measurable change in throughput**; the edge was unaffected (p99 2.0 ms).

### 2026-10-28, `8fcfd76`: M2 inference, normal and control

Both runs: 20 s idle at 20/s, then 1,000/s (ramp 10 s, hold 60 s), with ingest, attribution and
inference running; load-test model profile (10 ms, timeout 200 ms, capacity 4).

**Normal (4 workers): model latency stayed flat.** Model p95 was 11 ms idle and 11 ms across
60,000 spike messages, with no timeouts. Time to insight rose from 16 ms to 149 s (p95) and
recovered: the waiting happened in the stream. Inference lag (messages waiting):

```
   20s      0
   40s   8885 ##############
   60s  22794 ######################################
   90s  43741 ########################################################################   spike ends
  120s  38575 ################################################################
  150s  30137 ##################################################
  180s  21453 ###################################
  210s  13052 #####################
  240s   4588 #######
  250s   1680 ##                                                                           drained ~262s
```

**Control (200 workers): the model overloaded.** Model p95 rose from 12 ms to 190 ms (16×),
29,610 calls timed out and were retried, and 10 messages ran out of retries and were
dead-lettered. **With 50 times the workers, inference drained in 321 s instead of 162 s:**
retries spent model capacity on calls that failed again (congestion collapse). The bounded
worker pool is what prevents this.

**The edge was unaffected in both** (0 errors, p99 3.6 ms), as designed.

**Attribution drained more slowly than in M1** (154 s here against 27–37 s). JetStream keeps
the consumers' queues separate, but both services share one Postgres and one CPU: inference
writes about 400 insights/s, plus an existence check each, alongside attribution's writes. Run
to run variance is also large (an earlier run on `4ed6e5b` drained attribution in 41 s). A
resource-level bulkhead, such as separate connection limits or databases, would be the next
step if attribution's drain time mattered.

**Not in the table:** the first full run, on `4ed6e5b`, failed 2 of 71,372 requests. That led
to finding and fixing the ingest server's 10-second idle timeout in `8fcfd76` (see the
[M2 plan](plan-m2.md#deviations-from-the-plan)); both runs above are after the fix.

### 2026-10-28, `b0c2515`: after the M2 code-review fixes

Re-run because three fixes changed how results are measured: model latency is now fractional
(not whole milliseconds), the drain wait checks row counts, and the windows start at the first
interaction.

**Normal: unchanged.** Model p95 11.85 ms idle and 11.66 ms in the spike; time to insight p95 148 s;
zero loss, nothing dead-lettered.

**Control: overloaded again, but much slower to drain** than the earlier control run: inference
837 s (was 321 s), attribution 682 s (was 90 s), and 0 dead letters (was 10). None of the fixes
changed what the services do in control mode, so this is most likely variance amplified by
overload: 200 workers retrying against the same Postgres and CPU slow everything, attribution
included, by amounts that differ a lot between runs. One run of each isn't enough to say more.
It does reinforce the earlier note: the stream keeps the queues separate, but protecting
attribution from an overloaded neighbour would need a resource-level bulkhead too.
