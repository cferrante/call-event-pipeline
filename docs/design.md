# Design spec: decoupled lead ingestion layer

Status: **draft for review** · Stack: Go + NATS JetStream + Postgres (all native on Ubuntu/WSL2)

New to the terms used here (stream, consumer, ack, …)? See the [concepts guide](concepts.md).

## 1. Goals

1. Accept voice and SMS webhooks and acknowledge them fast enough that a traffic spike never
   causes the provider to time out or the edge to fall over.
2. Never lose an accepted event, and never double-count one (provider retries are expected).
3. Attribute every interaction to a campaign/source within milliseconds of it being processed.
4. Isolate downstream AI inference so that webhook spikes show up as **queue lag**, not as
   slower or failing inference.

## 2. Non-goals (for now)

- Real Twilio account / public URL (we simulate Twilio-shaped webhooks locally).
- Real AI model calls (milestone 2 uses a stub with configurable latency).
- Multi-node NATS cluster, Kubernetes, cloud deployment, auth on dashboards.
- Call recording / transcription.
- Ingesting outbound calls/SMS (needed to tell replies apart from new contacts; see
  [section 12](#12-decisions-and-known-limitations)).

## 3. Milestones

Milestones are referred to as M1, M2, …

| Milestone | Scope | Done when |
|---|-------|-----------|
| M1 | Ingest edge → JetStream → attribution consumer → Postgres, dead-letter path, k6 spike test | Spike test shows 0 lost, 0 duplicated interactions |
| M2 | AI inference consumer with bounded concurrency, priority lanes, stub model | Under spike, stub-inference p95 stays flat while consumer lag absorbs the burst, then drains |

## 4. Architecture

```
 webhook (voice | sms)
        │ POST, form-encoded, X-Twilio-Signature
        ▼
 ┌─────────────────────┐  verify signature → publish (wait for PubAck) → 200
 │ cmd/ingest          │  never touches Postgres
 └─────────┬───────────┘
           ▼
 ┌───────────────────────────────────────────┐
 │ JetStream stream LEADS                     │  file storage, dedupe by Nats-Msg-Id
 │ subjects: leads.raw.voice, leads.raw.sms   │
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

## 5. Components

### 5.1 Ingest edge (`cmd/ingest`)

- Go stdlib `net/http` (1.22+ pattern routing). Routes: `POST /webhooks/voice`, `POST /webhooks/sms`,
  `GET /healthz`.
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

### 5.2 JetStream configuration

| Setting | Value | Why |
|---|---|---|
| Stream `LEADS` subjects | `leads.raw.>` | One stream, channel in the subject |
| Storage | File | Survives restarts |
| Retention | Limits (max age 7d) | Lets new consumers (e.g. M2) replay history |
| Duplicate window | 10 min | Covers provider retry bursts; DB constraint covers beyond |
| Consumer `attribution` | Durable pull, `AckExplicit`, `AckWait 30s`, `MaxDeliver 5`, no `BackOff` | Pull lets workers fetch only what they can handle |
| Retry delays | `NakWithDelay` 1s, 5s, then 30s per attempt | JetStream's `BackOff` replaces `AckWait` with its first value, so a write slower than 1s would be redelivered while still running ([section 5.5](#55-delivery-guarantees)) |
| Stream `LEADS_DLQ` | Subject `leads.dlq`, file storage, max age 30d | Messages that can never succeed, kept for a human to inspect |

### 5.3 Attribution consumer (`cmd/attribution`)

- Fetches in batches, processes each message:
  - In one transaction:
    1. Insert into `interactions` `ON CONFLICT (provider_id) DO NOTHING`. If no row was
       inserted, this message was already processed: stop and change nothing else. This
       insert is the idempotency gate; see [section 5.5](#55-delivery-guarantees).
    2. Look up `To` in `tracking_numbers` → campaign/source/medium. No in-memory cache: it's a
       primary-key lookup inside a transaction we're already running, so it's always fresh.
       Unknown or inactive number → `source = 'unknown'` (not an error).
    3. Upsert the caller into `leads` by `From`, comparing `occurred_at` rather than processing
       order: first-touch takes the earliest interaction, last-touch the latest. *Why:*
       redelivery and parallel workers mean a later call can be processed before an earlier one.
       Withheld caller ID → skip the lead; the interaction keeps `lead_id = NULL`, so the
       campaign still gets credit. Otherwise set the interaction's `lead_id`.
  - Commit → `Ack`.
  - A cache is added only if the spike test ([section 8](#8-measuring-absorbing-spikes)) shows
    the lookup is a bottleneck.
- Error handling:
  - **Transient** (DB down, timeout) → `Nak` with delay; JetStream redelivers.
  - **Permanent** (unparseable envelope) → publish to `leads.dlq`, then `Term`.
  - **Retries exhausted** → on the final attempt (`MaxDeliver` = 5) the worker publishes to
    `leads.dlq` itself, then `Term`. It doesn't rely on JetStream's max-deliveries advisory,
    which is an unstored message lost if no listener is running at that moment.

### 5.4 Inference consumer (M2, sketch only)

- Separate durable consumer `inference` on the same stream, with its own pace.
- A fixed worker pool (e.g. N = model capacity) and `MaxAckPending` = N, so it never holds
  more in-flight work than the model can serve.
- Priority: first-touch leads before re-analysis (likely two consumers with different pools).
- The model is a stub with configurable latency/error rate.

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

## 7. Data model (Postgres)

```sql
tracking_numbers (phone_e164 PK, campaign_id, source, medium, active)
leads            (id PK, phone_e164 UNIQUE, first_campaign_id, first_source, first_seen_at,
                  last_campaign_id, last_source, last_seen_at)
interactions     (id PK, provider_id UNIQUE, lead_id FK NULL, channel, from_e164, to_e164,
                  campaign_id, source, occurred_at, ingested_at, raw JSONB)
```

- `interactions` is the record of facts: one row per inbound call/SMS, with the campaign
  current *at the time*, never rewritten. Any attribution model (multi-touch included) can be
  derived from it with SQL.
- `leads` holds precomputed first/last touch, a shortcut for the most common questions.
- "Conversation" is a reporting concept, not a stored one: a query groups a caller's messages
  to the same tracking number, starting a new conversation after a 24h gap. Reports count
  conversations, so a chatty SMS thread doesn't count as many leads.

Migrations: plain numbered `.sql` files applied with `psql` (no migration tool to install).

## 8. Measuring "absorbing spikes"

| Metric | Source | M1 target (laptop) |
|---|---|---|
| Edge p99 latency | k6 | < 50 ms at 1,000 req/s |
| Edge error rate | k6 | 0% non-2xx (excluding deliberate bad signatures) |
| Consumer lag (`num_pending`) | NATS monitoring `:8222/jsz` | Rises during spike, returns to 0 after |
| Loss | k6 accepted count vs `interactions` rows | Equal |
| Duplicates | k6 replays ~5% of requests | No extra rows |
| Inference p95 (M2) | stub worker timings | Flat (±10%) between idle and spike |

Uses the monitoring endpoint built into NATS and the k6 summary, so no Prometheus/Grafana
install is needed.

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
cmd/ingest/        cmd/attribution/     (cmd/inference/ in M2)
internal/twilio/   signature verification
internal/envelope/ schema + parsing
internal/attribution/ lookup + DB writes
migrations/        loadtest/ (k6 scripts)   docs/
```

Libraries: `nats.go` (`jetstream` package), `pgx/v5`, stdlib `log/slog`. Config via env vars.

## 11. Security & privacy

- Signature verification on every webhook; body size cap.
- Phone numbers are PII: logs show only the last 4 digits.
- Twilio auth token comes from an env var and is never committed.

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
- **A worker crash during a message's final attempt** leaves that message undelivered but out of
  the DLQ: JetStream won't redeliver it, and no worker is left to dead-letter it. The same
  happens if the DLQ write itself fails on the final attempt. The message isn't lost (it stays
  in `LEADS` for 7 days) and consumer metrics show it. The upgrade path is to capture
  JetStream's max-deliveries advisories in their own stream and dead-letter from there.
