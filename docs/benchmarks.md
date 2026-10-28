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

Columns: **Load** is peak rate, ramp and hold. **Edge** latency is k6's view of the ingest
service. **Peak lag** is the attribution consumer's largest backlog, and **Drained after** is how
long after the spike ended it reached 0. **Rows** is interactions in Postgres, ✓ when it's at
least the unique webhooks that got a 200 (zero loss) and at most the unique webhooks sent (zero
duplicates).

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
