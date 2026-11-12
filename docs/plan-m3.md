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
  with `schema_version: 2`, and one with `channel: "fax"`, go to the DLQ on the first delivery
  (today `fax` would hit the database `CHECK` and be retried five times, so the test fails for
  the right reason).
- Commit: *Dead-letter unsupported schema versions and channels*.

### Step 4: SendGrid Inbound Parse signature (`internal/sendgrid`, unit)

- ECDSA (P-256) verification of the signature over timestamp and body, with SendGrid's public
  key from env; reject timestamps older than 5 minutes. Header names and the signed payload are
  confirmed against Twilio's API reference first, and the source recorded here.
- Tests first: valid signature accepted; tampered body, wrong key, missing header and stale
  timestamp rejected. Signing helper for tests and `sendhook` uses a generated key pair.
- Commit: *Verify SendGrid Inbound Parse signatures*.

### Step 5: Email channel at the edge, schema v1.1 (unit, integration)

- `POST /webhooks/sendgrid/inbound`: 1 MB cap, signature check, `Message-ID` as `provider_id`,
  attachments counted not kept, publish to `leads.raw.email` with `Schema-Version: 1.1`.
- Schema v1.1: `channel` adds `email`; optional `subject`, `attachment_count`; examples under
  `examples/v1.1/`. The v1.0 examples must still validate.
- `sendhook -channel email`.
- Tests first: 403 on a bad signature, 400 without a `Message-ID`, 413 over the cap, published
  envelope validates against v1.1, a retried email is deduplicated.
- Commit: *Accept inbound email through a SendGrid-shaped webhook*.

### Step 6: Attribute and classify email (integration)

- Migration `0006`: `tracking_addresses`; `interactions.channel` allows `email`; `to_e164`
  nullable; `to_address`; `from_number` renamed `from_contact`. The conversations view
  updated to match.
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

None yet.

## Code-review outcomes

None yet.

## Results

To be filled in from [benchmarks](benchmarks.md) as steps 12, 15 and 16 complete.
