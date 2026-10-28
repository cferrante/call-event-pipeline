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
- **Step 5 was done before step 4.** They're independent: step 4's handler uses a fake
  publisher, and step 5 builds the real one.
- **Step 5: integration tests start their own `nats-server`** (`internal/natstest`), using the
  installed binary on a free port with a temporary data directory, and skip if it isn't
  installed. Each test gets a clean JetStream, and you don't have to start NATS first.
  Embedding the server as a Go library was rejected because it pulls the whole server into
  the module's dependencies.
- **Step 5: "publish twice → one message" is joined by "existing stream with an old config is
  updated".** Creating an identical stream twice already succeeds in JetStream, so a plain
  idempotence test couldn't tell a correct implementation from a wrong one.
- **Step 4: the signed URL is built from a configured public base URL**, not from the request's
  own host and scheme. Behind a tunnel or proxy the server sees e.g. `http://localhost:8081`
  while Twilio signed `https://<public host>`, so every signature would fail.
- **Step 4: two responses the spec didn't list.** A signed webhook that can't become an event
  (e.g. no `CallSid`) gets 400, because a retry would fail the same way. Publishing has a
  5-second deadline, well inside Twilio's 15-second timeout, so a stalled stream can't hold
  requests open.
- **Step 4: the handler returns whatever TwiML it's given.** Choosing it (e.g. `<Dial>` to a
  forwarding number) moves to step 10 with the rest of the wiring, as does the
  `TWILIO_AUTH_TOKEN` setup.
- **Step 6: the app connects as a least-privilege `leads_app` role**, not the developer's
  superuser login. `scripts/db-setup.sh` creates it and the `leads` database it owns. It has
  `CREATEDB` so tests can create throwaway databases; a production role wouldn't.
- **Step 6: `.env` / `.env.example` arrived now instead of step 10**, because `leads_app`'s
  password lives in `DATABASE_URL`.
- **Step 6: migrations run from Go (`internal/migrate`, `cmd/migrate`), not a `psql` script.**
  The SQL files are embedded in the binary and recorded in `schema_migrations`, so the
  migrator could be built test-first and runs anywhere without `psql`. An advisory lock lets
  services start at the same time safely.
- **Step 6: each integration test gets its own throwaway database** (`internal/dbtest`)
  instead of a shared `leads_test`, for the same reasons as one NATS server per test.
- **Step 6: `interactions.from_e164` is named `from_number`**, because for anonymous callers
  it holds words like `anonymous`, which aren't E.164 numbers.
- **Step 7: the interaction is inserted before the lead is touched**, reversing the spec's
  original order, so a redelivered message is a complete no-op. See
  [delivery guarantees](design.md#55-delivery-guarantees).
- **Step 7: the tracking-number lookup is part of the interaction insert** (one SQL statement
  with a `LEFT JOIN`), not a separate query. The insert needs the campaign anyway, and one
  statement keeps the gate and the lookup in a single round trip.
- **Step 8: no JetStream `BackOff`; retry delays come from `NakWithDelay`.** With `BackOff`
  set, JetStream uses its first value (1s) instead of `AckWait`, so a write slower than 1s is
  redelivered while still running. A test showed this against the real server (2 deliveries
  of a 1.5s write); the spec's [JetStream table](design.md#52-jetstream-configuration) is
  corrected.
- **Step 8: the consumer logic lives in `internal/consumer`**, so it can be tested against a
  real NATS server; `cmd/attribution` only wires it up (step 10).
- **Step 8: each attempt is limited to 20s,** below the 30s `AckWait`, so a stuck attempt fails
  and is retried by us instead of being redelivered while it's still running.
- **Step 9: the worker dead-letters on the final attempt itself, instead of listening for
  JetStream's max-deliveries advisory.** Advisories aren't stored, so any sent while the
  service is down or restarting would be missed. The remaining gap (a crash *during* the final
  attempt) is a known limitation in the
  [spec](design.md#12-decisions-and-known-limitations), with the upgrade path.
- **Step 10 is split into two commits:** 10a makes the ingest service runnable, 10b the
  attribution service, the end-to-end test and the README.
- **Step 10a: `cmd/sendhook`, a dev tool that sends correctly signed fake webhooks.** `curl`
  can't conveniently compute Twilio's signature, and the click-through steps need one.
- **Step 10a: `scripts/env-setup.sh` creates `.env` or adds variables it's missing**, turning
  `change-me` placeholders into random secrets without touching existing values.
  `db-setup.sh` previously only filled placeholders when creating `.env`, so an existing `.env`
  would never have gained the new variables.
- **Step 10a: optional `INGEST_FORWARD_TO`** chooses the voice TwiML (`<Dial>` or end the call),
  and the HTTP server has read/write timeouts so stalled clients can't tie up connections
  during a spike.
- **Step 10b: the attribution service applies migrations at startup**, so a fresh database
  needs no separate migrate step (`cmd/migrate` still exists for doing it by hand).
- **Step 10b: end-to-end tests (`e2e/`) build and run the real binaries** against a throwaway
  NATS server and database, which also covers the `main` wiring: config, startup migrations
  and clean exit on Ctrl-C.
- **Step 11: `loadtest/run.sh` runs the spike in isolation**: a fresh `leads_loadtest` database
  and its own NATS server on separate ports, both removed afterwards, so about 70,000 test rows
  never touch the dev database. It decides PASS/FAIL itself and exits non-zero on failure.
- **Step 11: k6 uses an arrival-rate executor**, so load doesn't drop when the server slows
  (no coordinated omission); see the [concepts guide](concepts.md#coordinated-omission).
- **Step 11: `run.sh` reads NATS's JSON stats with a two-line `python3` snippet**, because `jq`
  isn't installed and `grep` on JSON would be fragile. It's a test helper, not a service.
- **Step 11: benchmark history lives in [`docs/benchmarks.md`](benchmarks.md)**, one row per
  notable run with the git commit and machine, since this plan becomes history after M1.
  `run.sh` prints a ready-to-paste row; it never edits tracked files itself.
- **Step 12: the report is a database view, `campaign_conversations`** (migration `0002`), so
  anyone can query it without copying SQL around. A conversation spans channels: a call and a
  text to the same tracking number within 24 hours are one contact. Exactly 24 hours apart
  still counts as the same conversation.

## Code-review outcomes

Step 13 ran `/code-review` (medium effort) on the whole M1 branch: 10 findings, 8 fixed and 2
declined. Each fix was made test-first, with a deliberate break where a test could show it.

1. **Fixed: on shutdown, buffered messages were processed with the cancelled shutdown context**,
   so each failed and used up a retry; on a 5th attempt even the dead-letter write failed. `Run`
   now stops fetching on shutdown but processes messages it already holds with a context the
   shutdown can't cancel. Test: 29 of 30 buffered messages failed before the fix, 0 after.
5. **Fixed (attribution): `defer nc.Drain()` didn't wait**, so the last acks could be lost at exit,
   causing avoidable redeliveries. The service now waits until the connection has closed.
   Ingest is unaffected, since its publishes already wait for their PubAck. Not covered by a test:
   whether unsent acks are lost at exit depends on timing, so a test couldn't reliably show it.

2. **Fixed: a failed dead-letter write on the final attempt stranded the message**, because
   JetStream had no deliveries left to retry it. The consumer now allows one spare delivery
   (`MaxDeliver` 6 for 5 attempts), which also covers a worker crashing during attempt 5. Test:
   the DLQ stream is really deleted before attempt 5 and recreated before attempt 6; before the
   fix there was no attempt 6. The spec's known limitation narrows to two failures in a row.

3. **Fixed: the client buffered up to 500 messages but processed them one at a time.** `AckWait`
   counts down while a message waits in the buffer, so with slow writes (e.g. 100 ms) buffered
   messages would expire, be redelivered, use up retries, and could be dead-lettered with no
   real error. The client now pulls 10 at a time. Test: with a 100-message backlog the client
   held 100 unacked messages before the fix and at most 11 (10 buffered + 1 in progress) after.
   Effect on throughput is measured in [benchmarks](benchmarks.md).

Found while fixing these: **Go's test cache doesn't track the binaries the e2e tests build**, so
`go test -tags integration ./...` reported a stale `ok` after `cmd/attribution` changed. The README
now runs integration tests with `-count=1`.

## Results

### Spike test

Baseline at `2f7278d`, `./loadtest/run.sh` with the defaults (ramp to 1,000 req/s over 10s, hold
60s, 5% deliberate duplicates). **PASS.** Details, the lag curve and later runs are in
[benchmarks](benchmarks.md).

| Target ([spec section 8](design.md#8-measuring-absorbing-spikes)) | Result | |
|---|---|---|
| Edge p99 latency < 50 ms at 1,000 req/s | p50 0.42 ms, **p99 1.03 ms**, max 8.9 ms | ✓ |
| Edge error rate 0% | 0 of 70,945 requests failed | ✓ |
| Consumer lag rises during the spike, returns to 0 | peaked at **22,675** messages, drained 27s after the spike | ✓ |
| Loss: accepted = rows | 67,499 unique webhooks accepted, 67,499 interactions | ✓ |
| Duplicates: replays add no rows | 3,446 duplicates sent, 0 extra rows | ✓ |
| Dead letters | 0 | ✓ |

The spike became lag, not errors or lost leads. The consumer handles roughly 625–750 messages/s
one at a time; if draining ever needs to be faster, processing messages in parallel is the next
lever, measured with the same script before and after.
