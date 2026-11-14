# Postmortem: attribution acknowledged calls without storing them

**Date:** 2026-11-14 · **Duration of the fault:** 12.4 s · **Detected after:** 6.5 s ·
**Data lost after recovery:** none · **Status:** resolved

This was a **drill**: the fault was injected on purpose, on the AWS demo environment
([design section 8.3](../design.md#83-fault-injection-and-recovery-m3)), to test that a silent
failure is noticed and repaired. It is written up as if it were real, blamelessly: the
questions are what the system and the procedure did, not who did what.

## Summary

For 12.4 seconds the attribution service acknowledged every incoming call to the stream but
wrote none of them to the database. Nothing looked wrong from the outside: the process stayed
up, its health check passed and its backlog stayed at zero, because acknowledged messages count
as done. 621 calls arrived in that time and were missing from the database.

The monitor's *acknowledged but not written* rule emailed an alert 6.5 s after the fault began.
Once attribution wrote normally again, the missing calls were replayed from the stream, which
keeps every event for 7 days. The replay restored all 621 calls in 2.5 s, and a second replay
found nothing left to write.

## Impact

- **621 calls** (of 7,649 in the drill, sent at 50 per second) had no row in `interactions`
  from 15:58:23 until the replay about two minutes later, so they had no campaign attribution
  and were missing from the lead records. Anyone reading the data then would have under-counted
  those 12 seconds of calls.
- **Webhook senders were unaffected:** every webhook got a 200, because ingest only waits for
  the stream, and the stream had stored them all.
- **Classification:** inference reads the stream through its own consumer and wasn't faulted.
  We assume it classified these calls as normal; this drill didn't count their insights.
- **Nothing was lost permanently.** Every call was in the stream, so recovery was a replay.

## Timeline (UTC)

From the system under test's journal (`journalctl`, microsecond timestamps) and the mail
sink. Times marked ≈ come from the drill script's sequence, not from a log.

| Time | Event |
|---|---|
| 15:57:44.40 | Attribution restarted with its `faultinject` build (preparation; fault off) |
| ≈15:57:53 | Load starts: signed voice webhooks at 50/s from the load generator |
| **15:58:23.68** | **Fault on:** attribution acknowledges events without writing them |
| **15:58:30.14** | **Alert** `[FIRING] acked_not_written (attribution)`: "2038 acknowledged but 1715 written or dead-lettered in the last 1m0s". Email delivered at the same moment |
| 15:58:36.09 | Fault off: attribution writes again (mitigation) |
| 15:58:45.14 | Second alert `[FIRING] arrivals_stopped`: "no canary stored for 30s" (see below) |
| 15:58:50.14 | `arrivals_stopped` resolved, after 5 s |
| 15:59:30.15 | `acked_not_written` resolved: the gap has left the rule's 1-minute window |
| ≈16:00:24 | Load ends; both consumers drain |
| ≈16:00:40 | Replay `attribution -replay-from 2026-11-14T15:58:18Z` (5 s before the fault): 623 recorded in 2.5 s. A second replay records 0 |
| 16:01:06.31 | Normal attribution build restored |

**Time to detect:** 6.5 s, the next 5-second reading after the gap was big enough to trip the
rule (written under 90% of acknowledged over the last minute, with at least 100 acknowledged).
**Time to mitigate:** 12.4 s. **Time to recover the data:** about 2 minutes after the fault,
dominated by waiting for the load to end; the replay itself took 2.5 s.

The replay recorded 623 rather than 621 because it also restored the monitor's own canary
calls, which attribution writes like any other call (about one every 10 seconds).

## What went well

- **The silent failure was caught quickly by the one rule designed for it.** Lag, health
  checks and error rates all looked normal; only comparing acknowledgements with rows written
  could see it.
- **The stream made recovery exact.** The replay writes only what is missing, so starting it a
  few seconds early is safe, and a second replay proved nothing was left.
- **The edge was unaffected:** no sender saw an error and none had to retry.
- **No false alerts led up to it.** Since the ack-counting fix
  ([#33](https://github.com/cferrante/call-event-pipeline/pull/33)), a control run that overloads
  the model raised only the correct `stale_backlog` alert.

## What went wrong, or could have

- **A second alert pointed the wrong way.** `arrivals_stopped` fired because the canary calls
  were also acknowledged and not stored, and the rule only sees that no canary reached the
  database. Its name says arrivals stopped, but the edge was fine. On a real night, a
  responder could start by investigating ingest.
- **"Resolved" doesn't mean recovered.** `acked_not_written` resolved 54 s after the fault
  stopped, when the gap left its window, while all 621 calls were still missing. The email
  doesn't say that a replay is still needed, or from when.
- **Recovery is manual.** The responder must choose a replay time and run a command on the
  server. Nothing records that the replay was done.
- **We assume classification was unaffected** but didn't measure it in this drill.

## Follow-ups

| # | Action | Why | Where |
|---|---|---|---|
| 1 | Alert emails for `acked_not_written` include the replay command, with `-replay-from` set a minute before the alert's window started | Removes the guesswork from recovery and says plainly that resolving isn't recovering | M4 candidate |
| 2 | Rename or reword `arrivals_stopped` to "canary not stored (edge or attribution)" | So the second alert points at both possible causes | M4 candidate |
| 3 | A reconciliation check: insights with no matching interaction | A second, independent signal for this failure, which also confirms recovery | M4 candidate |
| 4 | The demo runbook walks through this drill and the replay | So someone other than the author can recover | [Step 20](../plan-m3.md#step-20-demo-runbook-concepts-and-status) |
| 5 | Count insights in the drill | Turns the assumption about classification into a measurement | M4 candidate |
