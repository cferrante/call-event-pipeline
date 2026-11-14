# Design spec: decoupled lead ingestion layer

Status: **agreed; milestones 1 and 2 implemented, milestone 3 specified** · Stack: Go + NATS JetStream + Postgres (all native on Ubuntu/WSL2; an on-demand AWS environment for server measurements)

New to the terms used here (stream, consumer, ack, …)? See the [concepts guide](concepts.md).

## 1. Goals

1. Accept voice and SMS webhooks and acknowledge them fast enough that a traffic spike never
   causes the provider to time out or the edge to fall over.
2. Never lose an accepted event, and never double-count one (provider retries are expected).
3. Attribute every interaction to a campaign/source within milliseconds of it being processed.
4. Isolate downstream AI inference so that webhook spikes show up as **queue lag**, not as
   slower or failing inference.
5. Detect a **silent failure** (a component that stays up but stops doing its job) within a
   minute, and recover from it without losing data.
6. Let the event schema evolve without a producer change silently breaking a consumer owned by
   another team.
7. Prove goals 1–5 with measurements on server hardware, not only on a laptop.

## 2. Non-goals (for now)

- Real Twilio or SendGrid accounts / public URL (we simulate their webhooks and APIs against
  their documented contracts).
- Real AI model calls **in load tests**: they use a stub with configurable latency (M2), whose
  profile M3 calibrates from a real model measured at a low rate ([section 5.4](#54-inference-consumer-cmdinference)).
- Multi-node NATS cluster, Kubernetes, production cloud deployment, auth on dashboards. M3 adds
  an **on-demand** AWS environment for demos and measurements, created and destroyed per
  session ([section 13](#13-demo-environment-aws)).
- Call recording / transcription.
- Ingesting outbound calls/SMS (needed to tell replies apart from new contacts; see
  [section 12](#12-decisions-and-known-limitations)).

## 3. Milestones

Milestones are referred to as M1, M2, …

| Milestone | Scope | Done when |
|---|-------|-----------|
| M1 | Ingest edge → JetStream → attribution consumer → Postgres, dead-letter path, k6 spike test | Spike test shows 0 lost, 0 duplicated interactions |
| M2 | AI inference consumer with bounded concurrency, stub model | Under spike, stub-inference p95 stays flat while consumer lag absorbs the burst, then drains |
| M3 | Schema contract and email channel, silent-failure monitor with alerting and replay recovery, crash test, on-demand AWS environment, server benchmarks, decision record, pitch ([plan](plan-m3.md)) | A silent failure is detected within 60 s and recovered with zero loss; repeated `kill -9` under load leaves data identical to a clean run; contract tests catch a breaking schema change; results measured on AWS |
| M4 (candidate) | Priority lanes with the transactional outbox; batched and parallel attribution | Chosen from M3's profile and throughput measurements |

## 4. Architecture

```
 webhook (voice | sms | email)
        │ POST; X-Twilio-Signature (voice, SMS) or SendGrid ECDSA signature (email)
        ▼
 ┌─────────────────────┐  verify signature → publish (wait for PubAck) → 200
 │ cmd/ingest          │  never touches Postgres
 └─────────┬───────────┘
           ▼
 ┌───────────────────────────────────────────┐
 │ JetStream stream LEADS                     │  file storage, dedupe by Nats-Msg-Id
 │ subjects: leads.raw.{voice,sms,email}      │
 └───────┬───────────────────────────┬───────┘
         │ durable consumer          │ durable consumer (M2)
         │ "attribution"             │ "inference"
         ▼                           ▼
 ┌─────────────────┐       ┌─────────────────────┐
 │ cmd/attribution │       │ cmd/inference (M2)  │
 └────────┬────────┘       └──────────┬──────────┘
          ▼                           ▼
       Postgres  ◄────────────────────┘
          │
 permanent failures ──► leads.dlq (stream LEADS_DLQ)
```

Each consumer is independent: a slow inference consumer cannot slow attribution, and neither
can slow the edge, because the stream sits between them.

Beside the data path (M3), `cmd/monitor` watches it from outside: it reads NATS's monitoring
endpoint and Postgres, raises alerts through an `Alerter`, and serves a live status page. In
demos, alert emails go to `cmd/mailsink`, a stand-in for SendGrid's Mail Send API.

```
 NATS :8222 (jsz) ──┐                     ┌──► log (always)
                    ├──► cmd/monitor ─────┼──► SendGrid Mail Send API (or cmd/mailsink)
 Postgres ──────────┘     status page     └──► (optional) Amazon SES
```

## 5. Components

### 5.1 Ingest edge (`cmd/ingest`)

- Go stdlib `net/http` (1.22+ pattern routing). Routes: `POST /webhooks/voice`, `POST /webhooks/sms`,
  `POST /webhooks/sendgrid/inbound` (M3), `GET /healthz`.
- Steps per request:
  1. Cap the body size (`http.MaxBytesReader`, 64 KB).
  2. Verify `X-Twilio-Signature` (HMAC-SHA1 over full URL + sorted POST params, base64).
     Invalid → `403`. *Why:* anyone can POST to a public URL; unsigned traffic must not
     reach the stream.
  3. Build the envelope ([section 6](#6-event-schema)) and publish to `leads.raw.<channel>` with
     `Nats-Msg-Id = <CallSid|MessageSid>`, **waiting for the PubAck**.
  4. PubAck received → `200` with a static TwiML response. Publish failed → `503` (provider
     retries; dedupe makes that safe).
- *Why wait for PubAck before 200:* it's the moment the event is durable on disk. Returning
  200 earlier would mean a crash could silently lose a lead.
- *Voice caveat:* a voice webhook's response tells Twilio what to do with the live call, so it
  must stay static and instant (e.g. `<Dial>` to a configured number). The edge never waits
  on attribution or AI to decide call routing.
- **Email (M3)** follows SendGrid's Inbound Parse contract: a `multipart/form-data` POST with
  `from`, `to`, `subject`, `text`, `html`, `headers` (the raw header text), `envelope` (JSON)
  and attachment file parts.
  - **Size:** SendGrid accepts emails up to 30 MB with attachments, so the edge accepts that
    much rather than rejecting (and losing) a lead who attached a file. The body is
    **streamed**: attachment parts are read and discarded without being buffered, while every
    byte still feeds the signature hash. Text fields are capped (64 KB each) and the envelope
    records only the attachment count, which keeps the stream small and attachment content
    (often PII) out of it. The event keeps one value per field within a 96 KB total (subject
    998 bytes, other small fields 4 KB each, then text, then headers), so even with JSON's
    6-byte escapes it stays well inside NATS's 1 MB message limit; a larger event could never
    be published, and SendGrid would retry it until the lead was lost.
  - **Signature:** SendGrid's security policy signs with ECDSA (P-256) over SHA-256 of the
    `X-Twilio-Email-Event-Webhook-Timestamp` header's bytes followed by the raw body, and sends
    the base64 ASN.1 signature in `X-Twilio-Email-Event-Webhook-Signature`. The public key
    (base64 DER) comes from env. Invalid → `403`. The timestamp must be all digits: with no
    separator in the signed bytes, that stops body bytes being shifted into it.
  - **No freshness window.** SendGrid documents no timestamp tolerance and doesn't say whether
    a retried delivery is re-signed, so rejecting old timestamps could drop a legitimate retry.
    A replayed request can only resend an email that was already accepted, and `Message-ID`
    deduplication makes that a no-op.
  - `provider_id` is the email's `Message-ID`, parsed from the raw `headers` field (Inbound
    Parse has no dedicated field), the email equivalent of a `MessageSid`, so provider retries
    dedupe exactly as for SMS. A message without one is rejected with `400`, since it couldn't
    be deduplicated.

### 5.2 JetStream configuration

| Setting | Value | Why |
|---|---|---|
| Stream `LEADS` subjects | `leads.raw.>` | One stream, channel in the subject |
| Storage | File | Survives restarts |
| Retention | Limits (max age 7d) | Lets new consumers (e.g. M2) replay history |
| Duplicate window | 10 min | Covers provider retry bursts; DB constraint covers beyond |
| Consumer `attribution` | Durable pull, `AckExplicit`, `AckWait 30s`, `MaxDeliver 6` (5 attempts + 1 spare), no `BackOff` | Pull lets workers fetch only what they can handle |
| Client pull batch | 10 messages (`PullMaxMessages`) | `AckWait` counts down while a message waits in the client's buffer; a small batch can always be processed within it, even with slow writes |
| Retry delays | `NakWithDelay` 1s, 5s, then 30s per attempt | JetStream's `BackOff` replaces `AckWait` with its first value, so a write slower than 1s would be redelivered while still running ([section 5.5](#55-delivery-guarantees)) |
| Stream `LEADS_DLQ` | Subject `leads.dlq`, file storage, max age 30d | Messages that can never succeed, kept for a human to inspect |

### 5.3 Attribution consumer (`cmd/attribution`)

- Fetches in batches, processes each message:
  - In one transaction:
    1. Insert into `interactions` `ON CONFLICT (provider_id) DO NOTHING`. If no row was
       inserted, this message was already processed: stop and change nothing else. This
       insert is the idempotency gate; see [section 5.5](#55-delivery-guarantees).
    2. Look up `To` in `tracking_numbers` → campaign/source/medium (for email, in
       `tracking_addresses`, M3). No in-memory cache: it's a
       primary-key lookup inside a transaction we're already running, so it's always fresh.
       Unknown or inactive number → `source = 'unknown'` (not an error).
    3. Upsert the caller into `leads` by `From`, comparing `occurred_at` rather than processing
       order: first-touch takes the earliest interaction, last-touch the latest. *Why:*
       redelivery and parallel workers mean a later call can be processed before an earlier one.
       Withheld caller ID → skip the lead; the interaction keeps `lead_id = NULL`, so the
       campaign still gets credit. Otherwise set the interaction's `lead_id`. Email senders
       get no lead in M3 either, because leads are keyed by phone number; matching one person
       across channels is later work ([section 12](#12-decisions-and-known-limitations)).
  - Commit → `Ack`.
  - A cache is added only if the spike test ([section 8](#8-measuring-absorbing-spikes)) shows
    the lookup is a bottleneck.
- Error handling:
  - **Transient** (DB down, timeout) → `Nak` with delay; JetStream redelivers.
  - **Permanent** (unparseable envelope) → publish to `leads.dlq`, then `Term`.
  - **Retries exhausted** → when attempt 5 (or later) fails, the worker publishes to
    `leads.dlq` itself, then `Term`. It doesn't rely on JetStream's max-deliveries advisory,
    which is an unstored message lost if no listener is running at that moment. The consumer
    allows one spare delivery (`MaxDeliver` 6), so if that dead-letter write fails, or the
    worker crashes during attempt 5, the message is delivered once more and dead-lettered then.
  - **Unsupported schema version or channel** (M3) → dead letter at once, with the reason, so
    an upgraded consumer can replay it ([section 6](#6-event-schema)).
- **Replay (M3):** `attribution -replay-from <RFC 3339 time>` creates a temporary consumer that
  starts at that time, reprocesses everything up to the stream's last message as of the
  start (later ones are the durable consumer's), then deletes itself; if the process dies
  first, the server removes it after 5 idle minutes. Messages that can't be parsed are skipped
  and counted, as they were dead-lettered the first time. The durable `attribution` consumer
  is untouched, so the service keeps running during a replay. Because every write is idempotent, already
  written messages are no-ops, so replay is the recovery for any period when writes went
  missing ([section 8.3](#83-fault-injection-and-recovery-m3)).

### 5.4 Inference consumer (`cmd/inference`)

Added in milestone 2 ([plan](plan-m2.md)). Its job is to keep AI inference at normal speed during
a webhook spike, letting work wait in the stream instead.

- **The model** is a stub in M2 (`internal/inference`); a real one can replace it behind the
  same `Model` interface. It classifies each interaction's intent (`sales`, `support` or
  `unknown`) with a score, from the SMS body, using fixed keyword rules.
- **The stub behaves like a real inference server under load.** It has a **capacity** of 4
  concurrent calls and a **base latency** of 200 ms (±10% jitter). A call that starts while
  *k* calls are in flight (itself included) takes `base × max(1, k / capacity)`: beyond
  capacity, calls share the model and each gets slower. If that would exceed the **timeout**
  (2 s), the call waits the timeout and fails as overloaded. Without this, *any* consumer would
  show flat latency and M2's experiment would prove nothing.
- **A separate durable consumer, `inference`**, on the `LEADS` stream, with its own progress, so
  a slow model never holds back attribution, and vice versa (the bulkhead pattern).
- **A worker pool of N = 4 workers**, matching the model's capacity, and the consumer's
  **`MaxAckPending` = 4**, which JetStream enforces on the server however many inference
  processes run. During a spike, messages wait in the stream (lag), not inside the model.
- **Starts at new messages.** A newly created `inference` consumer reads only messages published
  from then on, so a first deployment doesn't spend model capacity on the stream's 7-day history
  (backfilling old leads would be a deliberate, separate step). Attribution still reads everything.
  JetStream can't change where an existing consumer starts, so `go run ./cmd/inference
  -reset-consumer` deletes and recreates it (discarding its progress).
- **Idempotent like attribution:** results go to `insights` with a unique `provider_id`. Before
  calling the model, a worker skips messages whose insight already exists, so redeliveries
  don't spend model capacity; the unique constraint catches the rare race.
- **Independent of attribution:** an insight can be stored before its interaction is, because
  the two consumers run at their own pace. So `insights` has no foreign key to `interactions`;
  they join on `provider_id`.
- **Errors** follow [section 5.3](#53-attribution-consumer-cmdattribution): overload and database
  errors are retried with delays; unparseable messages and exhausted retries go to the DLQ,
  marked with a `Dlq-Consumer` header since both consumers share it.
- **Email (M3):** the model classifies the subject and text, as it does an SMS body.
- **A real model (M3)** sits behind the same interface: a Groq adapter (standard-library HTTP,
  no SDK), with the API key read from SSM at run time. On a hosted model, capacity is a rate
  limit rather than a number of concurrent calls, so a tokens-per-minute limiter runs beside
  the worker pool, and a `429` that still arrives is retried after `retry-after`. The limiter
  also keeps a **daily token budget** (`GROQ_TOKENS_PER_DAY`), because the free tier's daily
  limit is reported in no response header: a run that would exceed it fails fast instead of
  being refused halfway. The default model is `openai/gpt-oss-20b`, chosen on measured
  accuracy, latency, token cost and Groq's production status
  ([benchmarks](benchmarks.md#model-calibration-m3)). Phone numbers
  and email addresses are stripped before any call, and only synthetic texts are ever sent.
  An interaction with no text (a call, or the monitor's canary) is stored as `unknown` with
  model version `no-text` without a call, and only the first 8,000 bytes of a long message are
  sent, less if needed so one call always fits the budget a minute.
  It is used for **calibration**: a low-rate run measures real p50/p95/p99 latency and where
  `429`s begin, and those numbers replace the stub's guessed load-test profile. A small
  labelled set gives a measured accuracy for both the real model and the stub's rules.
- **Priority lanes** (first-touch leads before repeat contacts) are a candidate for M4, where
  publishing "first touch" events safely needs the transactional outbox pattern.

### 5.5 Delivery guarantees

JetStream delivers each message **at least once**: never lost, occasionally more than once.
Consumers must therefore make a repeat delivery harmless.

**Why repeats happen.** A consumer commits to Postgres, then acks to JetStream. Those are two
systems, and nothing makes them succeed or fail together, so there is always a gap:

- The worker crashes after the commit but before the ack.
- The ack is lost on the network.
- Processing takes longer than `AckWait` (30s), e.g. while Postgres struggles during a spike.
  JetStream assumes the worker died and redelivers **while the first worker is still running**,
  so two workers can process the same message at the same time.

In each case JetStream redelivers a message whose effects are already in the database.

**Why not ack before writing.** That only moves the gap: a crash after the ack loses the lead
for good (at-most-once). A duplicate we can detect is better than a loss we can't, so we keep
commit-then-ack and make processing **idempotent**: running it twice has the same effect as
running it once ("effectively once"). True exactly-once delivery across two independent
systems isn't achievable in general.

**Two sources of duplicates, two guards.**

| Where | Cause | Guard |
|---|---|---|
| Into the stream | The provider retries a webhook | JetStream's `Nats-Msg-Id` duplicate window ([section 5.2](#52-jetstream-configuration)) |
| Out of the stream | Redelivery to a consumer | Idempotent writes in Postgres (below) |

The stream's window also expires (10 minutes), so the database is the final guard for both.

**The interaction insert is the gate.** Each consumer transaction starts with
`INSERT INTO interactions … ON CONFLICT (provider_id) DO NOTHING RETURNING id`:

- A row comes back → first delivery → go on to update the lead.
- No row → already processed → change nothing else.

Doing this *first* means a repeat delivery is a complete no-op. Updating the lead first would
re-apply its changes. That is harmless today, because re-applying the same timestamps changes
nothing, but only by coincidence: a later change such as a touch counter would silently
over-count. It also covers concurrent duplicates: if two workers insert the same
`provider_id` at once, Postgres makes the second wait for the first to commit, then its
`DO NOTHING` applies, so exactly one worker updates the lead.

**Proven under crashes (M3).** `loadtest/crash.sh` sends a fixed set of webhooks, each a pure
function of its sequence number, while a random service (`ingest`, `attribution`, `inference`
or `nats-server`) is killed with `kill -9` every 5–15 s from a seeded sequence, at least 20
times, and restarted by a supervisor (systemd on the server, a restart loop locally). k6
retries like a provider, with backoff until it gets a `200`, so every webhook is eventually
accepted. Pass: no missing or unexpected rows, and an identical checksum over the ordered key
fields of `interactions`, each one's lead and its insight, against the expected data computed
in SQL from the sequence numbers. `KILLS=0` is the clean run, which must give the same
checksum. A lead's first and last touch are left out: retries during an outage legitimately
change the order calls arrive in.

Killing the `nats-server` process loses nothing JetStream has acknowledged, because the
operating system still holds its writes. A **power loss** could lose writes since JetStream's
last disk sync (its `sync_interval`, documented default 2 minutes, verified when measured).
M3 measures what `sync_interval: always` costs in edge latency, and the
[decision record](decisions/0001-event-stream.md) records the trade-off.

## 6. Event schema

Raw envelope published to the stream (JSON):

```json
{
  "schema_version": 1,
  "channel": "voice | sms",
  "provider": "twilio",
  "provider_id": "CA… | SM…",
  "from": "+15551234567",
  "to": "+15557654321",
  "received_at": "2026-10-27T12:00:00.123Z",
  "params": { "...": "all original form params, for replay" }
}
```

*Why keep the raw params:* the stream is the source of truth, so later consumers (or a fixed
attribution bug) can reprocess history without asking the provider again.

### 6.1 The schema as a contract (M3)

The envelope is the interface between the team that owns ingestion and the teams that own
consumers. M3 makes it an explicit, tested contract.

- **The contract** is `schemas/lead-event/v1.schema.json` (JSON Schema 2020-12), with example
  events in `schemas/lead-event/examples/v1.<minor>/`. JSON Schema is language-neutral, so a
  consumer written in another language can validate against the same file.
- **`schema_version`** stays the **major** version, the number `1`. Changing its type to carry
  a minor version would itself break every consumer. Minor versions (`1.0`, `1.1`) are
  releases of the schema file, and the publisher also sends the full version as a
  `Schema-Version: 1.1` message header, for logs and debugging only. Consumers decide by the
  major version.
- **Compatibility rules:**
  - A minor version only adds: new optional fields, or new values a consumer is told to expect.
  - Consumers ignore fields they don't know (the tolerant reader).
  - A breaking change is a new major version, published alongside the old one until every
    consumer has moved.
  - A consumer that receives a major version, or a channel, it doesn't support dead-letters the
    message with the reason. It never crashes or guesses.
- **v1.1 adds `channel: "email"`.** Adding an enum value looks additive but isn't safe for a
  consumer that assumes only `voice` and `sms`. Our consumers already reject an unknown
  channel in `envelope.Parse` and dead-letter it on the first delivery; the contract tests now
  pin that behaviour, so a consumer that isn't upgraded parks email events in the DLQ for
  replay instead of losing or misreading them. Email-only fields (`subject`,
  `attachment_count`) are optional.
- **Tests:**
  - *Producer:* every envelope `ingest` builds validates against the current schema.
  - *Consumer contract:* each consumer processes every example of every minor version it
    supports, including one with an unknown field, and dead-letters an example with an
    unsupported major version or channel.
  - *Compatibility:* every example from every earlier minor version still validates against the
    current schema, so an edit that breaks old messages fails CI.
- *Why not Go struct tests alone:* they would tie the contract to Go. *Why not a contract
  broker such as Pact:* it adds a service to run, which isn't justified for two consumers in
  one repo.

## 7. Data model (Postgres)

```sql
tracking_numbers (phone_e164 PK, campaign_id, source, medium, active)
leads            (id PK, phone_e164 UNIQUE, first_campaign_id, first_source, first_seen_at,
                  last_campaign_id, last_source, last_seen_at)
interactions     (id PK, provider_id UNIQUE, lead_id FK NULL, channel, from_number, to_e164,
                  campaign_id, source, occurred_at, ingested_at, raw JSONB)
insights         (id PK, provider_id UNIQUE, intent, score, model_version, model_ms,
                  received_at, created_at)                                    -- M2
tracking_addresses (address PK, campaign_id, source, medium, active)          -- M3
```

M3 changes to `interactions` (migration `0006`, additive only): `channel` also allows
`email`; `to_e164` becomes nullable and a nullable `to_address` holds the lowercased tracking
address an email was sent to, with a check that exactly one of the two is set. `from_number`
keeps its name and holds the sender's address for email: renaming it would break instances
of the old code still running during a deploy, which a cosmetic change doesn't justify.

- `interactions` is the record of facts: one row per inbound call/SMS, with the campaign
  current *at the time*, never rewritten. Any attribution model (multi-touch included) can be
  derived from it with SQL.
- `leads` holds precomputed first/last touch, a shortcut for the most common questions.
- `insights` holds one AI result per interaction (M2). `model_ms` is model latency;
  `created_at − received_at` is time to insight. No foreign key to `interactions`, because the
  inference consumer may store an insight before attribution stores its interaction.
- "Conversation" is a reporting concept, not a stored one: a query groups a caller's messages
  to the same tracking number, starting a new conversation after a 24h gap. Reports count
  conversations, so a chatty SMS thread doesn't count as many leads. It's the
  `campaign_conversations` view (conversations and interactions per campaign).

Migrations: plain numbered `.sql` files applied with `psql` (no migration tool to install).

## 8. Measuring "absorbing spikes"

| Metric | Source | M1 target (laptop) |
|---|---|---|
| Edge p99 latency | k6 | < 50 ms at 1,000 req/s |
| Edge error rate | k6 | 0% non-2xx (excluding deliberate bad signatures) |
| Consumer lag (`num_pending`) | NATS monitoring `:8222/jsz` | Rises during spike, returns to 0 after |
| Loss | k6 accepted count vs `interactions` rows | Equal |
| Duplicates | k6 replays ~5% of requests | No extra rows |
| Model latency p95 (M2) | `insights.model_ms` | Flat (±10%) between idle and spike |
| Time to insight p95 (M2) | `insights.created_at − received_at` | Rises during spike, recovers after |
| Inference loss/duplicates (M2) | `insights` rows vs accepted webhooks | Same checks as `interactions` |
| Control run (M2) | same spike, worker limit removed | Model visibly overloaded: latency up, timeouts |

Uses the monitoring endpoint built into NATS and the k6 summary, so no Prometheus/Grafana
install is needed.

**M3 measurements**, recorded in [benchmarks](benchmarks.md) with the machine and method. The
server rows use the environment in [section 13](#13-demo-environment-aws).

| Measurement | Target or expectation |
|---|---|
| The M1/M2 1,000/s spike, on AWS | Same pass criteria: 0 lost, 0 duplicated, model p95 flat (±10%) |
| Edge step-up test | Highest sustained rate with p99 < 50 ms and 0 errors; load generator CPU < 80% throughout, so the result measures the server, not k6 |
| Attribution sustained throughput | Measured, with the bottleneck named |
| Real model calibration (low rate) | p50/p95/p99 latency and the rate where `429`s start; the stub profile updated to match; accuracy on a labelled set |
| CPU profile (`pprof`) of `ingest` and `attribution` during the spike | Share of time in our code versus waiting on I/O; the evidence for or against optimising code (or rewriting it in another language) |
| Crash test | Identical counts and checksum after at least 20 kills |
| Monitor | Detects the injected failure within 60 s; **0 alerts** during a normal spike run |
| Replay recovery | Rows after replay equal the clean run |
| `sync_interval: always` | Edge p99 against the default |

### 8.1 Monitor and alerting (M3)

A watchdog that alerts on a normal traffic spike gets ignored, so lag alone is never treated as
a failure.

`cmd/monitor` evaluates these rules every 5 seconds:

| Rule | Detects | Fires when |
|---|---|---|
| **Arrivals stopped** | The edge or the provider is down | No **canary** row for 30 s. A canary is a signed synthetic voice webhook sent every 10 s from a reserved number to a reserved tracking number; reports exclude it. |
| **Volume drop** | A partial outage upstream | The last 5 minutes' arrival rate is below 20% of normal, where normal is the **median** of the per-minute rates over the hour before, and is at least 1/s. The median, not the mean: a sustained burst lifts an hour's mean several-fold, so ordinary traffic afterwards would look like an outage |
| **Acked but not written** | A consumer that stays up and acknowledges messages without writing them | Over 60 s, rows written are below 90% of messages the consumer acknowledged (from its ack floor), with at least 100 acknowledged |
| **Stale backlog** | A consumer falling too far behind | The oldest pending message is more than 5 minutes old |
| **Readings failing** | The monitor itself can't see the pipeline (Postgres or NATS down) | Every reading for 30 s has failed. Alerts already firing stay as they are until a reading succeeds; a failed reading is never treated as a reading of zero |

- **Alerts** have firing and resolved states, are logged as structured events, and appear on
  the status page. They go through an `Alerter` interface with three implementations: log
  (always on), SendGrid Mail Send API v3, and optionally Amazon SES. An alerter that fails or
  is slow never delays the rules: alerts are queued and sent with retry and backoff on `429`
  and `5xx`.
- **Status page:** arrivals per second, consumer lag and oldest pending age, model p95, time
  to insight, alert history, and an **Outbox** of alert emails with each one's delivery state
  (queued, sent, retrying, failed, or dropped because the queue was full).
  Served on `localhost` only; there is no auth, so it is never exposed publicly.

### 8.2 Mail sink (M3)

`cmd/mailsink` stands in for SendGrid's Mail Send API, so the alert path can be shown
end-to-end without an account (SendGrid no longer has a free plan, only a 60-day trial). It
accepts `POST /v3/mail/send`, checks the bearer token and the request's shape against
SendGrid's documented contract, returns `202` as SendGrid does, and keeps the last 100
messages, listed newest first at `GET /messages` on its loopback address. A real account is a
configuration change: `SENDGRID_BASE_URL` and the API key. Email alerts are off unless
`ALERT_EMAIL_TO` is set, and `SENDGRID_BASE_URL` has no default, so a development machine
never sends through real SendGrid by accident.

### 8.3 Fault injection and recovery (M3)

- Fault injection exists only in binaries built with the `faultinject` build tag, so a normal
  build can't contain it. In that build, `SIGUSR1` toggles `attribution` between normal and
  **acknowledge without writing**: the process stays up, its health check passes and its lag
  stays at zero, while nothing reaches Postgres. A test checks that a normal build contains
  none of this code. `MONITOR_WRITE_WINDOW` shortens the rule's minute-long window for a quick
  drill or test.
- Recovery is replay from the stream ([section 5.3](#53-attribution-consumer-cmdattribution)):
  fix the cause, then `attribution -replay-from` a time just before the incident started.
- A blameless postmortem of an injected incident on the server is written in
  `docs/postmortems/`: timeline from logs, time to detect, impact (rows missing), recovery,
  and follow-ups.

## 9. Local runtime (native, no Docker)

| Service | Install | Start / stop |
|---|---|---|
| Go 1.22+ toolchain | official tarball to `/usr/local/go` | — |
| nats-server | `go install github.com/nats-io/nats-server/v2@latest` | `nats-server -js -sd ./data/nats -m 8222` / Ctrl-C |
| Postgres | `apt install postgresql` | `sudo service postgresql start` / `stop` |
| k6 | Grafana's apt repo (`.deb`) | run per test |

Disk space is checked before each install. All of it is free and open source.

## 10. Repo layout

```
cmd/ingest/        cmd/attribution/     cmd/inference/ (M2)
cmd/monitor/       cmd/mailsink/        (M3)
internal/twilio/   signature verification
internal/sendgrid/ Inbound Parse signature verification, Mail Send client (M3)
internal/envelope/ schema + parsing
internal/attribution/ lookup + DB writes
internal/monitor/  rules and alerters (M3)
schemas/lead-event/ JSON Schema contract and examples (M3)
infra/             AWS bootstrap scripts and OpenTofu demo environment (M3)
migrations/        loadtest/ (k6 scripts)   docs/ (decisions/, postmortems/ from M3)
```

Libraries: `nats.go` (`jetstream` package), `pgx/v5`, stdlib `log/slog`. Config via env vars.

## 11. Security & privacy

- Signature verification on every webhook; body size cap.
- Phone numbers are PII: logs show only the last 4 digits. Email addresses are PII too: logs
  show only the domain.
- Twilio auth token comes from an env var and is never committed. So do SendGrid's public key
  and API key, and the alert recipient address; on AWS they come from SSM Parameter Store
  (SecureString), never from the instance's user data or the OpenTofu state.
- The demo environment opens **no inbound ports**; see [section 13](#13-demo-environment-aws).

## 12. Decisions and known limitations

Decisions:

1. **Attribution model:** store every interaction as a fact; keep first-touch and last-touch
   per caller on `leads`, chosen by `occurred_at`. Anonymous callers get no lead.
2. **Tracking-number lookup:** no cache; look it up in the write transaction. Revisit only if
   measurements show a bottleneck.
3. **SMS replies / repeat contacts:** every inbound message is stored; grouping into
   conversations happens at query time ([section 7](#7-data-model-postgres)).

Known limitations (deliberately out of M1):

- **No attribution window:** first-touch is kept forever; no 90-day "lead episodes".
- **Number reuse:** `tracking_numbers` has no `valid_from`/`valid_to`, so re-attributing old
  interactions would use the *current* mapping. Live attribution is unaffected because each
  interaction stores its campaign at write time.
- **Replies to our own outreach** look like inbound contacts until outbound calls/SMS are
  ingested.
- **Two failures in a row at the end of a message's retries** (e.g. the dead-letter write fails
  on attempt 5 *and* on the spare delivery, or a worker crashes during both) leave the message
  undelivered but out of the DLQ. It isn't lost (it stays in `LEADS` for 7 days) and consumer
  metrics show it. The upgrade path is to capture JetStream's max-deliveries advisories in
  their own stream and dead-letter from there.

Decisions and limitations added in M3:

- **The event stream:** NATS JetStream against Kafka (self-managed, MSK, MSK Serverless) and
  Kinesis, including cost at the measured load, is argued in
  [decision record 0001](decisions/0001-event-stream.md). The same record covers rewriting hot
  paths in C++: rejected unless the CPU profile shows our own code, not I/O, dominates.
- **No cross-channel identity:** an email sender and a caller who are the same person are not
  linked, and email senders get no `leads` row.
- **Power loss** can lose JetStream writes since its last disk sync
  ([section 5.5](#55-delivery-guarantees)); the cost of syncing every write is measured.
- **The demo runs in the default VPC.** Its subnets are public and its network ACL is open, so
  the security groups alone keep traffic out (no address range is allowed in, enforced before
  every apply), and each instance has a public IP to reach Systems Manager. That is acceptable
  for a dedicated account with nothing else in it and instances that live under 3 hours. In
  production, use a dedicated VPC with private subnets and Systems Manager VPC endpoints (about
  $0.03/hour for three) or a NAT gateway (about $0.045/hour plus per-GB charges).
- **An email to several tracking addresses credits one campaign.** The event takes the first
  SMTP recipient, and `provider_id` is the bare `Message-ID`, so a second address in the same
  delivery, or a separate delivery of the same message to another address, is dropped as a
  duplicate. The sender chooses the `Message-ID`, so reusing another email's exact ID would
  also drop that email. Both are rare: a lead seldom writes to two of a business's tracking
  addresses at once, and the second needs another email's ID. The fix, an M4 candidate, is one
  event per tracking address with `provider_id` = `Message-ID` plus that address, which
  changes the schema contract (found in the M3 code review).
- **The monitor is a single process.** If it dies, nothing alerts. In production it would run
  outside the system it watches, with its own heartbeat to an external service ("who watches
  the watchdog").

## 13. Demo environment (AWS)

An on-demand environment for server measurements and live demos. It exists only while in use,
so it costs nothing between sessions.

- **Account:** its own AWS account, `call-event-pipeline`, in the existing AWS Organization.
  *Why:* a clean cost signal, a small blast radius, and teardown by closing the account.
  Created with idempotent scripts in `infra/bootstrap/aws/`, which the owner runs once: the
  account, sign-in through IAM Identity Center, and a **$10/month budget** with email alerts at
  50% actual, 100% actual and 100% forecast. Member accounts in an Organization have no AWS free
  tier.
- **Why the budget alone isn't enough:** budget data updates only a few times a day, so it can
  show cost but can't stop it. The guard is that **every instance terminates itself after 3
  hours**: it is created with shutdown-means-terminate and schedules `shutdown -h +180` at boot.
- **Infrastructure:** OpenTofu with local, git-ignored state, in `infra/demo/`. `tofu destroy`
  removes everything it created; `infra/demo/check.sh` (read-only) lists anything tagged
  `project=call-event-pipeline` that still exists.
- **Two instances** in the default VPC, us-east-1:

  | Role | Type | Runs |
  |---|---|---|
  | System under test | `c7g.xlarge` (4 vCPU, 8 GB, Graviton) | `nats-server`, Postgres (apt), the services and `monitor`, under systemd |
  | Load generator | `c7g.large` (2 vCPU, 4 GB) | k6, `sendhook`, the canary |

  *Why a separate load generator:* generating load on the machine being measured takes CPU
  from it and distorts the result.
- **No inbound ports from any address.** The only ingress rule lets the load generator's
  security group reach ingest's port. Access is through AWS Systems Manager (Run Command for
  scripts, Session Manager for a shell and for port-forwarding the status page to
  `localhost`). `make demo-up` refuses any plan that allows an address range in
  ([infra/demo](../infra/demo/README.md)).
- **Build and deploy:** binaries are cross-compiled locally for `linux/arm64` and shipped
  through a short-lived S3 bucket, so the private repository is never cloned onto the server.
- **Commands:** `make demo-up`, `demo-spike`, `demo-crash`, `demo-fault`, `demo-down`, with a
  10-minute walkthrough in `docs/demo.md`.
- **Cost** (on-demand us-east-1, about $0.145/h + $0.0725/h, plus two public IPv4 addresses
  and disks): about $0.23 an hour, so a 2-hour session is about $0.50. Rates are checked
  against AWS's price list when the account is created.
