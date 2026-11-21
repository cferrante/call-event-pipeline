# Demo: a 10-minute walkthrough

A live demonstration on the [AWS demo environment](../infra/demo/README.md): a 1,000/s spike
absorbed with nothing lost, then a silent failure caught by email and repaired by replay. The
[pitch](pitch.md) is the story to tell alongside it; the [benchmarks](benchmarks.md) hold the
full measurements.

**Cost:** about $0.23 an hour while the environment is up. A rehearsal plus the demo takes
under an hour, so about $0.25; at most $0.70, because every instance terminates itself after
3 hours.

## Before the audience arrives (about 10 minutes)

Needs the AWS bootstrap done ([infra/bootstrap/aws](../infra/bootstrap/aws/README.md)),
OpenTofu, Go, and the Session Manager plugin
([how to install it](../infra/demo/README.md#use)).

1. **Terminal 1:** sign in and create the environment.

   ```bash
   set -a; source .env; set +a
   aws sso login --profile <PROJECT_PROFILE>
   make demo-up
   ```

   Expected: after 2–6 minutes, the smoke test's checks are all ✓ and it ends with
   `Demo environment up in … s. Instances terminate themselves 180 minutes after first boot.` If the browser sign-in page opens, sign in there first.
2. **Terminal 2:** forward the status page and alert inbox.

   ```bash
   set -a; source .env; set +a
   make demo-status
   ```

   Expected: `Status page:  http://localhost:18090` and `Alert inbox:
   http://localhost:18025/messages`, then it keeps running. Leave it open.
3. **Browser:** open both addresses side by side. Expected: the status page's banner says
   **Healthy: no alerts**, *Arrivals* ticks up every 10 seconds (the [canary](concepts.md#canary)),
   and the inbox is an empty list.

## The walkthrough

### 1. What it is (1 minute)

Show the picture from the [README](../README.md#how-it-works). Three sentences:

- Calls, texts and emails arrive as webhooks; the edge stores each one in a durable stream and
  answers in about a millisecond.
- Two independent workers read the stream at their own pace: one credits each contact to its
  campaign, the other classifies it with an AI model.
- A separate monitor watches the results from outside and emails when something is wrong.

### 2. A spike is absorbed (3 minutes)

In terminal 1:

```bash
make demo-spike
```

What to point at while it runs (about 2 minutes):

- **Terminal:** the backlog lines rise to roughly 15,000 for attribution and 40,000 for
  inference during the 30 s at 1,000/s, then fall to 0. The stream holds the rush; the
  workers catch up.
- **Status page:** the same lag in the *Consumers* table, and the banner stays **Healthy**.
  A rush is not an incident.

Expected at the end: every check ✓, including *nothing lost or duplicated*, *every call
classified*, *model latency flat* and *no alert emails during the run*; edge p99 about 1 ms;
drained about a minute after the spike. The deliberate duplicates (5% of requests) show that
provider retries are stored once.

### 3. A silent failure is caught and repaired (5 minutes)

```bash
make demo-fault
```

Explain while it starts: attribution is swapped for a build that can be told to acknowledge
messages **without saving them**. It stays up and looks healthy, which is what makes this
kind of failure dangerous ([life of an incident](concepts.md#life-of-an-incident)).

- **30 s in, the fault turns on.** Within about 5–7 seconds the status page's banner turns to
  **1 alert(s) firing** with `acked_not_written (attribution)`, and an email titled
  `[FIRING] acked_not_written` appears in the inbox (refresh it). A second alert,
  `arrivals_stopped`, may follow briefly: the canary's calls were lost too.
- **The fault turns off** and the load continues to the end (about 2½ minutes in all).
- **Replay:** the script reads the stream again from just before the fault and restores the
  missing calls, in a few seconds. Then it replays again to show the second pass changes
  nothing.
- **Resolved:** about a minute after the fault ended, a `[RESOLVED]` email arrives.

Expected at the end: five ✓ checks: *detected within 60 s*, *the fault lost data* (proof the
drill really broke something; about 600 calls), *replay restored every row*, *a second replay
changed nothing*, *the alert resolved*.

### 4. What it proved (1 minute)

Close with the [pitch's table](pitch.md#what-we-proved): no loss in three 1,000/s rushes, 23
crashes with identical data, detection in seconds against a one-minute goal.

## Afterwards

1. **Terminal 2:** Ctrl-C stops the forwarding.
2. **Terminal 1:** remove everything.

   ```bash
   make demo-down
   ```

   Expected: OpenTofu destroys the resources, the demo's secrets are deleted, and the final
   check reports nothing left over (about a minute).
3. If anything looks left behind later: `make demo-check` lists it and exits 1.

## If something goes wrong

| Symptom | Fix |
|---|---|
| `no valid session for profile …` | `aws sso login --profile <PROJECT_PROFILE>`, then repeat the command |
| `needs the Session Manager plugin` | Install the Session Manager plugin ([demo README](../infra/demo/README.md#use)) |
| The status page doesn't load | Check terminal 2 is still running; restart `make demo-status` |
| A spike check fails | The run prints which one. Repeat it: each run has its own IDs. Then compare with [benchmarks](benchmarks.md) |
| Running out of time | `RATE=200 HOLD=10s make demo-spike` is a smaller, faster spike |

## Without AWS

The same story runs on a laptop at $0: start the services as in the
[local setup](running.md), send a spike with `./loadtest/run.sh`, and run the
[silent-failure drill](running.md#drill-a-silent-failure). The numbers differ from the server's; the
[benchmarks](benchmarks.md) say by how much.
