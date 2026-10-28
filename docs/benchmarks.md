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

Rows come from the same runs as the table above (`run.sh` prints both). **Mode** is `normal`
(workers = model capacity) or `control` (the limit removed). The load-test model profile is
10 ms latency, 200 ms timeout, capacity 4 (see the [M2 plan](plan-m2.md#deviations-from-the-plan)).
Model latency should stay flat in `normal`; time to insight rises during the spike by design.

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
