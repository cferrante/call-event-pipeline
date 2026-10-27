# Plan: milestone 1 (M1)

Status: **draft for review** · Spec: [design.md](design.md)

M1 goal: ingest edge → JetStream → attribution consumer → Postgres, with a dead-letter path and
a k6 spike test proving zero lost and zero duplicated interactions.

## How we work through this plan

- **One step at a time.** Each step is agreed before it starts.
- **Test-first.** Write the test, run it, and see it fail *for the right reason* before writing
  the code. When a test passes, break the code on purpose to prove the test catches it.
- **One commit per step**, tests green, with the diff shown before committing.
- **Two kinds of tests:**
  - *Unit tests* (`go test ./...`): pure Go, no services, run in milliseconds.
  - *Integration tests* (`go test -tags integration ./...`): run against the real local NATS
    and Postgres. *Why not mock them:* a mocked database can't catch a wrong SQL query or a
    misconfigured stream, and those are exactly the bugs that matter here.
- Deviations from this plan and code-review outcomes are recorded at the bottom.

## Steps

### Step 0: Install the local toolchain (no code)

Check `df -h /` first; stop if under 5 GB free. Rough disk use: ~0.5 GB in total.

| Tool | Install | Cost |
|---|---|---|
| Go (latest stable) | official tarball → `/usr/local/go` | free |
| nats-server | `go install github.com/nats-io/nats-server/v2@latest` | free |
| PostgreSQL | `sudo apt install postgresql` | free |
| k6 | Grafana's apt repository | free |

Done when: `go version`, `nats-server --version`, `psql --version` and `k6 version` all work,
and you can start and stop NATS and Postgres yourself.

### Step 1: Go module skeleton

- `go mod init`, the [repo layout](design.md#10-repo-layout), and one trivial test so
  `go test ./...` runs.
- Commit: *Initialise Go module and project layout*.

### Step 2: Twilio signature verification (`internal/twilio`, unit)

- Tests first:
  - Twilio's published example (URL, params, auth token → known signature) passes.
  - A tampered param, a wrong token and a missing header all fail.
  - Comparison is constant-time (`hmac.Equal`). *Why:* a normal string compare leaks how many
    leading characters matched, which lets an attacker guess a signature byte by byte.
- Commit: *Verify Twilio webhook signatures*.

### Step 3: Event envelope (`internal/envelope`, unit)

- Tests first:
  - Voice and SMS form params → envelope with the right `channel`, `provider_id`, `from`, `to`,
    and the original params kept.
  - Missing `CallSid`/`MessageSid` → error.
  - `Anonymous`/blank `From` is recognised as anonymous.
  - JSON round-trip, including `schema_version`.
- Commit: *Add lead event envelope*.

### Step 4: Ingest HTTP handler (`cmd/ingest`, unit)

- The handler depends on a small `Publisher` interface, so these tests use a fake publisher and
  `httptest`, without NATS.
- Tests first:
  - Bad signature → 403, and nothing published.
  - Body over 64 KB → 413.
  - Publisher error → 503.
  - Success → 200, the right TwiML for voice and SMS, published to `leads.raw.<channel>` with
    the message ID set to the provider ID.
- Commit: *Add ingest webhook handler*.

### Step 5: JetStream publisher and stream setup (integration)

- Creates or updates stream `LEADS` with the [spec's configuration](design.md#52-jetstream-configuration).
- Tests first:
  - Publish returns only after the PubAck.
  - Publishing the same message ID twice stores **one** message (the dedupe window works).
- Commit: *Publish lead events to JetStream*.

### Step 6: Database schema (`migrations/`)

- Numbered `.sql` files for the [data model](design.md#7-data-model-postgres), plus a small
  script that applies them with `psql`. A separate `leads_test` database is used for tests.
- Verified by applying the migrations twice to a fresh database (the second run must be a
  no-op or a clean error, never a half-applied state).
- Commit: *Add initial database schema*.

### Step 7: Attribution store (`internal/attribution`, integration)

- Tests first:
  - Known tracking number → correct campaign and source.
  - Unknown number → `source = 'unknown'`.
  - The same `provider_id` twice → one interaction.
  - **Out of order:** process the later call first, then the earlier one; first-touch and
    last-touch still come out right.
  - Anonymous caller → interaction with `lead_id = NULL`, no lead row.
- Commit: *Attribute interactions to campaigns idempotently*.

### Step 8: Attribution consumer (`cmd/attribution`, integration)

- Durable pull consumer, `Ack` after commit.
- Tests first:
  - A valid message ends up in Postgres and is acked.
  - An unparseable message goes to `leads.dlq` and is terminated, not retried.
  - A transient database error is retried, and the message succeeds once the database is back.
- Commit: *Consume lead events and dead-letter bad messages*.

### Step 9: Retries-exhausted → dead letter (integration)

- Listen for JetStream's max-deliveries advisory and copy the failed message to `leads.dlq`.
- Test first: a message that fails every attempt ends up in `leads.dlq` after `MaxDeliver`.
- Commit: *Dead-letter messages that exhaust retries*.

### Step 10: Runnable services and README

- Config from env vars, graceful shutdown on Ctrl-C (finish in-flight work first).
- README gains a **Running locally** section, now that the commands exist.
- Includes **click-through steps** for you: start the services, send a signed sample webhook
  with `curl`, and see the row in Postgres.
- Commit: *Wire up ingest and attribution services*.

### Step 11: Spike test (`loadtest/`)

- k6 script: ramps to ~1,000 req/s, signs each request, replays ~5% as duplicates.
- A verification query compares k6's accepted count with the `interactions` rows.
- Run it, watch consumer lag at `:8222/jsz`, and record the results against the
  [targets in the spec](design.md#8-measuring-absorbing-spikes) in this plan.
- Commit: *Add spike load test and record results*.

### Step 12: Conversation report query

- A SQL query grouping messages into conversations (24h gap), tested first with fixture data.
- Commit: *Add conversation reporting query*.

### Step 13: Code review

- Run `/code-review` on the M1 branch, fix or explicitly decline each finding, and record the
  outcomes below.

## Deviations from the plan

- **Step 1: directories are created when a step first needs them**, not all up front. Git
  doesn't track empty directories, so creating the whole layout now would mean committing
  placeholder files.
- **Step 1: the first test is `GET /healthz`**, not a throwaway test. It's real code the
  ingest service needs (see the [ingest edge in the spec](design.md#51-ingest-edge-cmdingest)).
- **Step 1: `INGEST_PORT` was added now instead of in step 10**, because port 8080 is already in
  use on the dev machine. It defaults to 8080 and rejects invalid values at startup.
- **Step 2: signatures are checked with and without the default port.** Twilio's docs say
  voice-over-HTTPS URLs are signed without the port but SMS URLs keep it, so the URL we
  rebuild may differ from the signed one by `:443`/`:80`. Twilio's own helper libraries do the
  same. A different *non-default* port is still rejected.
- **Step 3: "anonymous caller" is broader than the spec's `Anonymous`/blank.** Twilio sends a
  word such as `anonymous` or `unknown` in `From`, and has also used keypad numbers for those
  words (ANONYMOUS → `+266696687`). A caller is anonymous if `From` is blank, isn't an E.164
  number, or is one of those keypad numbers (derived in code from the words).
- **Step 3: `params` is stored as name → list of values** (Go's `url.Values`), not name →
  string as sketched in the [spec's event schema](design.md#6-event-schema), so a replay
  reproduces the exact form the signature was computed over.
- **Step 3: `envelope.Parse` validates events read from the stream.** Its errors mark messages
  that can never succeed, which step 8 sends to the dead-letter queue instead of retrying.

## Code-review outcomes

_None yet._

## Results

_Spike test results are recorded here in step 11._
