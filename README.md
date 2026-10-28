# Decoupled lead ingestion layer

Real-time ingestion for inbound voice and SMS leads: every call and text is captured and
attributed to its marketing campaign within milliseconds, and the system absorbs large spikes
in webhook traffic without slowing down downstream AI inference.

## How it works

Webhooks are acknowledged as soon as they're durably stored in a stream. All real work happens
afterwards, in independent consumers that each run at their own pace:

    webhook ─► ingest edge ─► NATS JetStream ─┬─► attribution ─► Postgres
                                              └─► AI inference (planned)

A traffic spike shows up as a temporary backlog in the stream, not as dropped webhooks or a
slower AI model.

New to NATS or JetStream? Start with the [concepts guide](docs/concepts.md).

## Stack

Go · NATS JetStream · PostgreSQL · k6 (load testing). Everything runs natively on Ubuntu
(WSL2), with no Docker, using free and open-source tools.

## Status

Milestone 1 in progress: the services run end to end and pass the spike test (see
[benchmarks](docs/benchmarks.md)); the conversation report and code review are next. See the [design spec](docs/design.md) for the architecture and decisions, and the
[M1 plan](docs/plan-m1.md) for progress.

- **M1:** ingest edge, attribution consumer, dead-letter handling, spike test proving zero
  loss and zero duplicates
- **M2:** AI inference consumer with backpressure, so inference latency stays flat under load

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
go run ./cmd/ingest                       # 3. webhook endpoint on $INGEST_PORT
```

Stop any of them with Ctrl-C; each finishes in-flight work first.

**Try it**

```bash
go run ./cmd/sendhook                 # a signed fake voice webhook → 200
go run ./cmd/sendhook -channel sms    # an SMS
go run ./cmd/sendhook -bad-signature  # → 403
psql "$DATABASE_URL" -c 'SELECT provider_id, source, lead_id FROM interactions'
psql "$DATABASE_URL" -c 'SELECT * FROM campaign_conversations'   # conversations per campaign
```

Calls to numbers missing from `tracking_numbers` are attributed to `unknown`. Add one with
`INSERT INTO tracking_numbers (phone_e164, campaign_id, source) VALUES ('+18005550001', 'search-fall', 'google');`

**Test**

```bash
go test ./...                     # unit tests; no services needed
go test -tags integration ./...   # plus integration and end-to-end tests; needs .env loaded
./loadtest/run.sh                 # spike test (needs k6), isolated from your dev data
```

Integration tests start their own NATS server and throwaway databases. Without `.env` loaded
they are skipped, which `go test` reports as `ok`; add `-v` to see `SKIP`.

## Why this project

A learning project for event-driven architecture and current best practices for absorbing
webhook spikes: durable streams, at-least-once delivery, idempotency, and backpressure.
