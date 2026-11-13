# Call event pipeline

Real-time ingestion for inbound voice, SMS and email leads: every call, text and email is captured and
attributed to its marketing campaign within milliseconds, and the system absorbs large spikes
in webhook traffic without slowing down downstream AI inference.

## How it works

Webhooks are acknowledged as soon as they're durably stored in a stream. All real work happens
afterwards, in independent consumers that each run at their own pace:

    webhook ─► ingest edge ─► NATS JetStream ─┬─► attribution ─► Postgres
                                              └─► AI inference ─► Postgres

A traffic spike shows up as a temporary backlog in the stream, not as dropped webhooks or a
slower AI model.

New to NATS or JetStream? Start with the [concepts guide](docs/concepts.md).

## Stack

Go · NATS JetStream · PostgreSQL · k6 (load testing). Everything runs natively on Ubuntu
(WSL2), with no Docker, using free and open-source tools.

## Status

Milestones 1 and 2 are complete: webhooks are ingested, attributed and classified by a (stub) AI
model, and the spike test shows zero loss and flat model latency at 1,000 webhooks/s (see
[benchmarks](docs/benchmarks.md)). See the [design spec](docs/design.md) for the architecture and
decisions, and the [M1](docs/plan-m1.md) and [M2](docs/plan-m2.md) plans for how each was built.

- **M1:** ingest edge, attribution consumer, dead-letter handling, spike test proving zero
  loss and zero duplicates
- **M2:** AI inference consumer with backpressure, so inference latency stays flat under load
- **M3 (in progress):** email channel, schema contract, silent-failure monitor with alert
  email, replay recovery, and a crash test; server benchmarks on AWS next. What each step
  proved is in the [M3 plan's progress table](docs/plan-m3.md#progress)

## Running locally

Needs Go, `nats-server` and PostgreSQL installed natively (see
[plan step 0](docs/plan-m1.md#step-0-install-the-local-toolchain-no-code)).

**One-time setup**

```bash
./scripts/db-setup.sh   # creates .env with random secrets, the leads_app role and database
```

**Run** (each in its own terminal, from the project folder; run `set -a; source .env; set +a`
in each terminal first)

```bash
nats-server -js -sd ./data/nats -m 8222   # 1. message broker, data kept in ./data
go run ./cmd/attribution                  # 2. applies migrations, then consumes events
go run ./cmd/inference                    # 3. classifies events with the (stub) AI model
go run ./cmd/ingest                       # 4. webhook endpoint on $INGEST_PORT
go run ./cmd/monitor                      # 5. alerts + status page on http://127.0.0.1:8090
go run ./cmd/mailsink                     # 6. receives the monitor's alert emails
```

Stop any of them with Ctrl-C; each finishes in-flight work first.

If inference refuses to start because its consumer "already exists with a different start
position" (one created before it started at new messages), run it once as
`go run ./cmd/inference -reset-consumer`.

The monitor sends a signed "canary" webhook every 10 seconds and alerts (in its log and on
its status page) if the canary stops arriving, traffic drops far below normal, a consumer
acknowledges messages without writing them, or a backlog stops draining. The page listens on
`127.0.0.1` only, since it has no login.

Alerts are also emailed through SendGrid's Mail Send API. Locally that is `cmd/mailsink`, which
checks each request against SendGrid's documented contract, answers `202` as SendGrid does, and
keeps the messages: `curl -s 127.0.0.1:8025/messages`. The status page's **Outbox** shows each
alert email and how delivery went (sent, retrying after a `429` or `5xx`, or failed). Unset
`ALERT_EMAIL_TO` to turn email off.

**Try it**

```bash
go run ./cmd/sendhook                 # a signed fake voice webhook → 200
go run ./cmd/sendhook -channel sms    # an SMS
go run ./cmd/sendhook -bad-signature  # → 403
psql "$DATABASE_URL" -c 'SELECT provider_id, source, lead_id FROM interactions'
psql "$DATABASE_URL" -c 'SELECT * FROM campaign_conversations'   # conversations per campaign
go run ./cmd/sendhook -channel sms -body "what's the price?"      # an SMS the model classifies as sales
psql "$DATABASE_URL" -c 'SELECT provider_id, intent, score, model_ms FROM insights'
go run ./cmd/sendhook -channel email -subject "Price?" -attach-bytes 200000  # a signed SendGrid-style email
psql "$DATABASE_URL" -c "SELECT provider_id, channel, to_address, source FROM interactions WHERE channel = 'email'"
```

Calls to numbers missing from `tracking_numbers` are attributed to `unknown`. Add one with
`INSERT INTO tracking_numbers (phone_e164, campaign_id, source) VALUES ('+18005550001', 'search-fall', 'google');`
Emails are credited through `tracking_addresses` the same way:
`INSERT INTO tracking_addresses (address, campaign_id, source) VALUES ('search-fall@leads.example.com', 'search-fall', 'google');`

Email is simulated against SendGrid's Inbound Parse contract: `scripts/env-setup.sh` generates a
key pair, `sendhook` signs with the private key and ingest verifies with the public one
(`SENDGRID_PUBLIC_KEY`; without it the email route is off).

**Drill: a silent failure**

A build with the `faultinject` tag lets you break attribution on purpose: `SIGUSR1` makes it
acknowledge events without writing them, so it stays up with zero lag while nothing reaches
Postgres. Normal builds don't contain this code.

```bash
go build -tags faultinject -o /tmp/attribution-drill ./cmd/attribution
/tmp/attribution-drill                      # in place of go run ./cmd/attribution
pkill -USR1 -f attribution-drill            # fault on; again to turn it off
```

With traffic flowing, the monitor fires *acked but not written* (and, 30 s later, *arrivals
stopped*, since the canary's rows aren't written either) and emails the alert to the mail sink.

To recover, turn the fault off and replay the stream from just before it started. Only the
missing rows are written, and attribution can keep running meanwhile:

```bash
go run ./cmd/attribution -replay-from 2026-11-13T09:30:00Z   # logs read, recorded, already_recorded
```

**Test**

```bash
go test ./...                     # unit tests; no services needed
go test -tags integration -count=1 ./...   # plus integration and end-to-end tests; needs .env loaded
./loadtest/run.sh                 # spike test (needs k6), isolated from your dev data
MODE=control ./loadtest/run.sh    # same spike without the inference limit: the model overloads
./loadtest/crash.sh               # kill -9 random services under load (~5 min); data must be exact
KILLS=0 ./loadtest/crash.sh       # the same load without kills: the clean run
```

Integration tests start their own NATS server and throwaway databases. `-count=1` turns off
Go's test cache, which can't see what these tests depend on (the services they build and run,
NATS, Postgres) and would otherwise report a stale `ok`. Without `.env` loaded
they are skipped, which `go test` reports as `ok`; add `-v` to see `SKIP`.

## Why this project

A learning project for event-driven architecture and current best practices for absorbing
webhook spikes: durable streams, at-least-once delivery, idempotency, and backpressure.
