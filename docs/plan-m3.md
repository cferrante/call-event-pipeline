# Plan: milestone 3 (M3)

Status: **agreed; in progress** · Spec: [design.md](design.md) · Previous: [M2 plan](plan-m2.md)

M3 goal ([spec goals 5–7](design.md#1-goals)): show that the pipeline holds up when things go
wrong, and measure it on server hardware. Done when:

- a silent failure is detected within 60 s and recovered by replay with zero data loss;
- repeated `kill -9` under load leaves the data identical to a clean run;
- contract tests catch a breaking change to the event schema;
- the spike, step-up, crash and monitor results are measured on AWS and recorded in
  [benchmarks](benchmarks.md);
- the decision record and the one-page pitch are written for their readers.

## How we work through this plan

As in M1 and M2, test-first with deliberate breaks, one logical change per commit and PR, and
deviations and review outcomes recorded at the bottom. Following the updated
[working agreement](../CLAUDE.md#how-we-work), steps run back to back without a checkpoint;
work stops only for a decision or a step marked 🧑, which the owner runs.

Documents that need review (the postmortem, decision record, pitch and this plan) are published
as a review page before they are committed.

## Decisions agreed before starting

| # | Decision | Why |
|---|---|---|
| D1 | Priority lanes and the transactional outbox move to M4 (candidate), with batched and parallel attribution | M3's scope is reliability and server proof; M3's profile and throughput numbers decide what M4 optimises |
| D2 | SendGrid is **simulated** against its documented contracts: Inbound Parse webhooks from `sendhook`, Mail Send through `cmd/mailsink` | SendGrid has no free plan, only a 60-day trial; a simulator is deterministic and needs no DNS changes. A real account is a configuration change. |
| D3 | The owner runs the one-time AWS bootstrap (account, sign-in, budget). Claude runs `demo-up`, `demo-down` and the server benchmarks within plan steps, always with the 3-hour self-termination in place | Keeps account-level changes with the owner, without a round trip per server session |
| D4 | Budget $10/month, email alerts at 50% actual, 100% actual and 100% forecast | Expected M3 spend is about $3–5 |
| D5 | No rewrite in another language; profile first, and argue the result in the decision record | The pipeline's time goes on I/O; the profile proves or disproves that |
| D6 | Load tests keep the stub, **calibrated** from a real model (Groq free tier) measured at a low rate | Load tests should measure the pipeline, not a provider's variance and rate limits; calibration replaces the stub's guessed latency with measured numbers |

## Steps

### Step 0: Agree the spec and this plan

On branch `m3-spec`; merged before any code. The spec changes are in
[design.md](design.md) (goals, milestones, sections 5, 6.1, 7, 8.1–8.3, 11–13).

### Step 1: Continuous integration

- GitHub Actions on every PR and push to `main`: `gofmt -l` (fails if any file needs
  formatting), `go vet`, unit tests, and integration tests against a natively installed
  `nats-server` (downloaded release binary, pinned version) and the runner's preinstalled
  Postgres. No containers.
- Test first: the first push includes a deliberately misformatted file, and the run must fail on
  `gofmt`; then the file is fixed in the same PR.
- Cost: a few minutes a run, inside the 2,000 free minutes a month for private repos.
- Commit: *Run formatting, vet and tests in CI*.

### Step 2: Schema contract v1.0 (`schemas/lead-event/`, unit)

- `v1.schema.json` describing today's envelope exactly; examples for voice, SMS and one with an
  unknown extra field under `examples/v1.0/`.
- Tests first: producer test (envelopes from `envelope.FromWebhook` validate); compatibility
  test (every example of every minor version validates against the current schema).
  Deliberate break: make `provider_id` required-and-renamed in the schema; both tests must fail.
- Test-only dependency: `github.com/santhosh-tekuri/jsonschema/v6`.
- Commit: *Publish the lead event schema as a tested contract*.

### Step 3: Consumers enforce the contract (integration)

- `Schema-Version` header on publish; consumers dead-letter an unsupported major version or an
  unknown channel at once, with the reason, instead of retrying it.
- Tests first: each consumer's contract test processes every example it supports; an example
  with `schema_version: 2`, and one with `channel: "fax"`, go to the DLQ on the first delivery.
- Commit: *Dead-letter unsupported schema versions and channels*.

### Step 4: SendGrid Inbound Parse signature (`internal/sendgrid`, unit)

- ECDSA (P-256) verification of the signature over timestamp and body, with SendGrid's public
  key from env. Header names and the signed payload confirmed against Twilio's docs (see the
  deviations below).
- Tests first: valid signature accepted; tampered body, wrong key, missing header and a
  non-numeric timestamp rejected. Signing helper for tests and `sendhook` uses a generated key pair.
- Commit: *Verify SendGrid Inbound Parse signatures*.

### Step 5: Email channel at the edge, schema v1.1 (unit, integration)

- `POST /webhooks/sendgrid/inbound`: 30 MB cap with the body streamed, signature check, `Message-ID` as `provider_id`,
  attachments counted not kept, publish to `leads.raw.email` with `Schema-Version: 1.1`.
- Schema v1.1: `channel` adds `email`; optional `subject`, `attachment_count`; examples under
  `examples/v1.1/`. The v1.0 examples must still validate.
- `sendhook -channel email`.
- Tests first: 403 on a bad signature, 400 without a `Message-ID`, 413 over 30 MB, an email
  with a large attachment accepted without buffering it, published
  envelope validates against v1.1, a retried email is deduplicated.
- Commit: *Accept inbound email through a SendGrid-shaped webhook*.

### Step 6: Attribute and classify email (integration)

- Migration `0006`: `tracking_addresses`; `interactions.channel` allows `email`; `to_e164`
  nullable; `to_address`. (`from_number` is not renamed; see deviations.)
- Attribution looks up email in `tracking_addresses`; no lead for email senders. Inference
  classifies subject and text.
- Tests first: an email is attributed to its campaign; an unknown address is `unknown`; an
  email produces an insight; existing voice/SMS tests unchanged; e2e covers all three channels.
- Commit: *Attribute and classify inbound email*.

### Step 7: Monitor rules (`internal/monitor`, unit)

- The four rules from [spec section 8.1](design.md#81-monitor-and-alerting-m3) as pure
  functions of sampled inputs (arrival counts, canary times, ack floor, rows written, oldest
  pending age), with firing/resolved state and a fake clock.
- Tests first: each rule fires on its failure pattern and resolves after; **none fires on the
  recorded M2 spike shape** (lag rising to 45,000 and draining).
- Commit: *Add monitor rules for silent failures*.

### Step 8: Monitor service, canary and status page (`cmd/monitor`, integration)

- Samples NATS `/jsz` and Postgres every 5 s; canary sender (signed voice webhook every 10 s
  from a reserved number; reports exclude it); status page on `localhost` with SSE updates.
- Tests first: with real NATS and Postgres, stopping `ingest` fires *arrivals stopped* within
  30 s and resolves on restart; a spike run produces no alerts.
- Commit: *Run the monitor with a canary and status page*.

### Step 9: Alerting and the mail sink (unit, integration)

- `Alerter` interface: log, SendGrid Mail Send v3 client, queue with retry and backoff on `429`
  and `5xx`. `cmd/mailsink` implements `POST /v3/mail/send` (bearer token, request shape, `202`)
  and feeds the status page's Outbox.
- Tests first: client request matches SendGrid's documented shape; `429` is retried; a hung
  alerter never delays rule evaluation; mailsink rejects a missing token (401) and a malformed
  body (400).
- Commit: *Send alert emails through a SendGrid-compatible API*.

### Step 10: Fault injection and detection (`faultinject` build tag, e2e)

- `SIGUSR1` toggles attribution's *acknowledge without writing* in tagged builds only.
- Tests first: e2e under steady load, the toggle fires *acked but not written* within 60 s and
  an alert email reaches the mailsink; a build without the tag doesn't contain the fault
  (checked by a test that the symbol is absent).
- Commit: *Inject a silent write failure for drills*.

### Step 11: Replay recovery (integration)

- `attribution -replay-from <time>`: temporary consumer from that time to the stream's end,
  then deleted; the durable consumer is untouched.
- Tests first: after an injected gap, replay restores row counts and checksum to a clean run's;
  replaying an already-complete period changes nothing.
- Commit: *Recover missing writes by replaying the stream*.

### Step 12: Crash test (`loadtest/crash.sh`)

- Seeded k6 scenario with fixed provider IDs, retrying like a provider until `200`; a supervisor
  restart loop locally; random `kill -9` of `ingest`, `attribution`, `inference` or
  `nats-server` every 5–15 s, at least 20 kills; checksum over ordered key fields.
- Deliberate break: switch attribution to ack before commit; the crash run must show loss.
- Local run recorded in [benchmarks](benchmarks.md).
- Commit: *Add a crash test under load*.

### Step 13: 🧑 AWS account, sign-in and budget

- Claude adapts cferrante-dev's bootstrap scripts into `infra/bootstrap/aws/` (account,
  Identity Center access, budget, and the Groq API key as an SSM SecureString) with a
  read-only `check.sh`, and checks the instance prices against AWS's price list.
- Test first: `check.sh` fails before the scripts are run.
- The owner runs the scripts (numbered steps provided); Claude confirms with `check.sh`.
- Commit: *Bootstrap the demo AWS account and budget*.

### Step 14: Demo environment (`infra/demo/`, OpenTofu)

- Two instances, default VPC, no inbound ports, SSM access, 3-hour self-termination, S3
  artifact bucket, systemd units, secrets from SSM Parameter Store; `make demo-up` /
  `demo-down`; `infra/demo/check.sh` lists leftovers.
- Tests first: `tofu validate` and a plan check that fails if any security group allows
  inbound traffic or an instance lacks the termination guard; a smoke test that fails until
  `/healthz` answers through Session Manager.
- After `demo-down`, `check.sh` reports nothing left.
- Commit: *Create the on-demand demo environment*.

### Step 15: Real model adapter and calibration

- A Groq adapter behind the `Model` interface (standard-library HTTP client, no SDK), selected
  by env; the API key is read from SSM at run time and never written to disk. Phone numbers
  and email addresses are stripped from text before any call; only synthetic texts are sent.
- A tokens-per-minute limiter beside the worker pool, so a hosted model's rate limit is
  respected instead of answered with `429`s; a `429` that still arrives is retried after the
  provider's `retry-after`.
- Calibration run at a low rate: real p50/p95/p99 latency and the point where `429`s start.
  The stub's load-test profile is replaced by these measurements.
- Accuracy: a small labelled set of synthetic SMS and email texts, scored against the real
  model and the stub's keyword rules.
- Tests first: the adapter against a local fake server (request shape, auth header, `429`
  with `retry-after`, timeout); the limiter never exceeds its budget (fake clock); redaction
  removes numbers and addresses. Deliberate break: disable the limiter and the fake server's
  `429` count must rise above zero.
- Cost: $0 on Groq's free tier; the run is paced well under its limit (8,000 tokens a minute,
  measured in cferrante-dev).
- Commit: *Calibrate the stub model against a real model*.

### Step 16: Server benchmarks

- On AWS: the 1,000/s spike (normal and control), the edge step-up test, attribution's
  sustained throughput, `pprof` CPU profiles of `ingest` and `attribution`, the crash test,
  monitor detection and false-alert check, replay recovery, and `sync_interval: always`.
- Recorded in [benchmarks](benchmarks.md) with machine, commit and method; the session's cost
  recorded too.
- Commit: *Record server benchmarks*.

### Step 17: Postmortem

- From an injected incident on the server: blameless, with timeline from logs, time to detect,
  impact, recovery by replay, and follow-ups. Published for review before committing.
- Commit: *Write the silent write failure postmortem*.

### Step 18: Decision record 0001: the event stream

- NATS JetStream against Kafka (self-managed, MSK, MSK Serverless) and Kinesis: operations,
  delivery semantics and cost at the measured load, with dated, sourced prices; the C++
  question with the profile as evidence. Published for review before committing.
- Commit: *Record the event stream decision*.

### Step 19: One-page pitch

- `docs/pitch.md` for a non-technical reader: what the pipeline protects, what M3 proved, the
  measured numbers. Published for review before committing.
- Commit: *Add a one-page pitch*.

### Step 20: Demo runbook, concepts and status

- `docs/demo.md` (10-minute walkthrough); concepts: contract test, tolerant reader, canary,
  reconciliation, replay, chaos test, sync interval, Session Manager, profile; README and
  `CLAUDE.md` project state.
- Click-through steps for the owner to run the demo end to end.
- Commit: *Document the M3 demo and concepts*.

### Step 21: Code review

`/code-review` on the M3 changes; fix or decline each finding and record outcomes below.

### Optional: real alert email through Amazon SES

`SESAlerter` behind the same interface, in SES's sandbox (verified recipient only). Only if a
real inbox moment is wanted in the demo; about $0.10 per 1,000 emails, checked first.

## Deviations from the plan

- **Step 1: integration tests can no longer pass by skipping in CI.** `dbtest` and `natstest`
  skip a test when Postgres or `nats-server` is missing, and Go reports a skipped test as
  `ok`, so a misconfigured CI job would have been green while testing nothing. New package
  `internal/testenv` turns those skips into failures when `REQUIRE_INTEGRATION=1`, which CI
  sets. Proven by running the attribution tests without `DATABASE_URL`: skipped (`ok`) by
  default, `FAIL` with the variable set.
- **Step 1: CI exposed a flaky test helper, fixed in this step.** The first CI run failed
  `TestConsumersProgressIndependently` (7 of 10 deliveries). The helper `waitSettled` treated
  "nothing pending" as done, but JetStream updates a consumer's pending count asynchronously,
  so under CPU contention it briefly reads 0 while messages are undelivered. Reproduced locally
  with 8 parallel runs on 2 cores (9 failures in 480 runs); the earlier local runs, on an idle
  8-core machine, never opened that window. The helper now waits until the consumer's ack
  floor reaches the stream's last sequence: 0 failures in 480 runs under the same contention.

- **Step 3: the unknown-channel behaviour already existed.** The spec and this step assumed an
  unknown channel would reach the database `CHECK` and be retried five times. In fact
  `envelope.Parse` already rejects it as permanent, so it is dead-lettered on the first
  delivery. The step became: the `Schema-Version` header (test failed first: header empty),
  contract tests through the real attribution and inference stores, and tests pinning the
  first-delivery dead letter. Those pinning tests passed on their first run, so each was
  proven with a deliberate break: a strict JSON reader failed both stores' contract tests on
  the unknown-field example, and retrying parse errors gave 5 deliveries instead of 1. The
  spec's [section 6.1](design.md#61-the-schema-as-a-contract-m3) is corrected.

- **Step 4: SendGrid's signing details, from its docs.** Sources: Twilio's "Securing your
  Inbound Parse Webhooks" (modified 2026-10-07), which defers to "Event Webhook security
  features" (modified 2026-07-23) for the mechanics, and "Setting up the Inbound Parse
  Webhook" (modified 2026-10-08) for the fields. Headers
  `X-Twilio-Email-Event-Webhook-Signature` / `-Timestamp`; ECDSA over SHA-256 of the timestamp
  bytes then the raw body, no separator; base64 ASN.1 signature; base64 DER public key. The
  docs don't name the curve: the example key's encoding identifies P-256, and a test pins that
  our keys encode with the same prefix.
- **Step 4: no timestamp freshness window.** The spec proposed rejecting timestamps older than
  5 minutes. The docs give no tolerance and don't say whether retries are re-signed, so a
  window risks rejecting legitimate retries and losing leads; replays are already harmless
  because of `Message-ID` deduplication. Instead the timestamp must be digits only: a test
  first written to show that moving bytes between timestamp and body breaks the signature
  was wrong, since the unseparated construction makes `"17615664001"+"23"` and
  `"1761566400"+"123"` the same bytes. Digits only closes that.
- **Step 4/5: 30 MB streamed instead of a 1 MB cap.** SendGrid accepts 30 MB emails with
  attachments; a 1 MB cap would reject them and lose those leads. The edge streams the body,
  discarding attachment parts unbuffered while hashing every byte for the signature.
- **Step 5: `Message-ID` comes from the raw `headers` field,** as Inbound Parse has no
  dedicated field.

- **Steps 5 and 6 swapped: consumers before the producer.** Publishing schema 1.1 adds an
  email example, and the consumer contract tests (step 3) require every published example to
  be processed, so they failed until attribution and inference supported email: the
  contract blocked releasing the producer change first, which is its job. Step 6 (schema 1.1,
  migration, email attribution and classification) therefore ships first and step 5 (the edge
  route and `sendhook`) second, the safe rollout order for a new event type.
- **Step 6: `from_number` is not renamed.** Renaming a column breaks instances of the old
  code still running during a deploy, and doing it safely needs an expand-and-contract
  migration; for a cosmetic change that isn't worth it. Migration `0006` is additive only.
- **Step 6: the conversations view is unchanged.** Email senders have no lead, and the view
  already counts every lead-less interaction as its own conversation.

- **Step 5: the email route extends its own read deadline to 60 s.** The server's 10-second
  `ReadTimeout` (set in M1 against slow clients) would cut off a 30 MB upload on a slow link;
  only this route is relaxed, through `http.ResponseController`, so calls and SMS keep the
  tight limit.
- **Step 5: the simulator key must be PKCS#8.** `openssl genpkey -outform DER` writes SEC1,
  which Go's PKCS#8 parser rejects; checking the generated key against the Go code before
  wiring it up caught this. `env-setup.sh` converts with `openssl pkcs8 -topk8`, and a test
  runs the script and checks the pair it writes.

- **Step 7: the volume rule's baseline is a median.** The spec compared the recent rate with
  the trailing hour's mean. A test of a 5-minute burst at 1,000/s (the step-up benchmark's
  kind of load) showed the mean-based rule raising a false alert afterwards; the median of
  per-minute rates doesn't. The recorded 70-second M2 spike alone was not enough to show this,
  which an earlier estimate here had wrongly assumed: over a full hour it only doubles the
  mean, so the sustained-burst test is what proves the choice.
- **Step 7: rules are tested against a simulator, not recorded data.** `loadtest/results/` is
  git-ignored, so the M2 run's samples aren't in the repo. A small model of the pipeline
  (arrival rate, per-consumer capacity) reproduces the recorded shape, and a test checks it
  does: inference lag peaks above 40,000, as measured (45,642).

- **Step 8: the status page polls instead of using server-sent events.** It fetches
  `/status` every 2 seconds, which looks live at a 5-second reading interval and needs no
  long-lived connection handling. Its script and styles are separate files so the page runs
  under a strict Content-Security-Policy with nothing inline.
- **Step 8: the monitor reads JetStream through its API, not the `:8222/jsz` HTTP endpoint.**
  The client gives the same numbers (stream last sequence, ack floors, the oldest pending
  message) without exposing or parsing the monitoring port.
- **Step 8: the canary comes from `From: canary`.** Not an E.164 number, so it never creates a
  lead; migration `0007` keeps it out of the campaign report.
- **Step 8: two defects found and fixed in passing.** The acked-but-not-written rule compared
  a consumer that appeared mid-window against zero (a realistic test, with the ack floor as a
  stream position, showed a false alert; the simulator's first version hid it because both
  counts started at zero). And ingest's defaults test read `SENDGRID_PUBLIC_KEY` from the
  shell, failing once `.env` had the key; it passed in step 5 and in CI only because neither
  environment had it yet.

- **Step 9: the Outbox shows the sender's view, not the mail sink's.** The plan had
  `cmd/mailsink` feed the status page. Instead the monitor's alert queue records each email's
  delivery (queued, sent, retrying with the last error, failed, dropped), and the sink lists
  what it received at `GET /messages`. The sender's view keeps working against real SendGrid,
  where there is no sink to ask, and it shows retries and failures, which the sink never sees.
- **Step 9: SendGrid's Mail Send contract, from its docs.** Sources: "Mail Send" (modified
  2025-09-25), "Rate limits" (2026-07-02) and "Responses" (2025-11-12). `POST /v3/mail/send`,
  `Authorization: Bearer`, `personalizations` (1 to 1,000, each with `to`) and `from` required,
  `202` on success, errors as `{"errors":[{"message","field"}]}`. A `429` carries
  `X-RateLimit-Reset`; the docs show epoch seconds in an example without stating the unit, so
  that is an inference. The queue waits until the reset (capped at the 60 s maximum backoff)
  before retrying.
- **Step 9: the integration is client-to-sink over HTTP, not a test against services.** Neither
  side touches NATS or Postgres, so a test of our client against the real sink handler on a
  local socket is the integration; the full path, monitor to sink, is step 10's e2e test. A
  manual run confirmed it: with inference stopped, *stale backlog* fired and its email reached
  the sink on the first attempt.
- **Step 9: a test that missed its deliberate break, fixed.** The ordering test failed the
  first delivery attempt, whichever event it was for. Delivering in parallel didn't break it,
  because Go's scheduler tends to run the newest goroutine first, so the *resolved* event took
  the failure and the two still arrived in order. The test now fails the *firing* event's first
  attempt, and parallel delivery fails it.

- **Found in step 10's CI run: e2e tests could seed a table before its migration ran.** The
  helper that starts attribution waited only for the `interactions` table (migration 0001),
  but each migration commits on its own, so the email test could insert into
  `tracking_addresses` (0006) before it existed. Locally the gap between the two is too short
  to hit; on a loaded CI runner it opened. Reproduced with 8 parallel runs on 2 cores (27
  failures in 120); the helper now waits until `schema_migrations` lists every embedded
  migration: 0 failures in 120 under the same contention.
- **Step 10: the e2e test shortens the rule's window to 3 s.** The rule needs a full window of
  readings from before the fault, so with the default minute the test would take well over a
  minute. A new `MONITOR_WRITE_WINDOW` setting shortens it, and the test detected the fault
  in about 0.4 s at 100 events/s (3 runs in parallel). The 60-second target with default
  settings is measured on the server (steps 16 and 17).
- **Step 10: the canary catches this fault too.** The canary's rows go through attribution,
  so while it acknowledges without writing, *arrivals stopped* also fires after its 30 s.
  The dedicated rule is faster and names the consumer; the canary is the end-to-end backstop.
- **Step 10: the symbol-absence test first checked the wrong name.** It looked for
  `main.silentWrites`, but Go names a method `main.(*silentWrites).Record`, so the check would
  have passed for any build. The test first asserts that the tagged build *does* contain the
  symbol, which exposed this; it now matches `silentWrites`, and a deliberate break (the fault
  compiled into every build) fails it.
- **Step 10: CI vets the tagged build,** so fault code that compiles only under `faultinject`
  can't break unnoticed.

- **Step 11: replay reads up to the last message as of its start.** "To the stream's end"
  would never end while traffic flows; the durable consumer owns anything published later.
  Its consumer has no acks and is deleted when done, with a 5-minute inactivity limit as the
  fallback if the process dies. Unparseable messages are skipped and counted (they were
  dead-lettered the first time) rather than dead-lettered again; a write that keeps failing
  stops the replay with an error, and re-running it is safe.
- **Step 11: the clean-run comparison includes each lead's first and last touch.** Replay
  writes the gap's events after later ones, so the test also shows that lead attribution
  doesn't depend on processing order. The new e2e tests ran 48 times without failure with 8
  parallel runs on 2 cores, the contention that exposed the earlier flake.

## Code-review outcomes

None yet.

## Results

To be filled in from [benchmarks](benchmarks.md) as steps 12, 15 and 16 complete.
