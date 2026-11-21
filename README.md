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

Milestones 1, 2 and 3 have met their goals; M3's final code review is its last step. Results
were measured on AWS as well as a laptop (see [benchmarks](docs/benchmarks.md)), and the
[pitch](docs/pitch.md) tells the story in one page. See the [design spec](docs/design.md) for the
architecture and decisions, and the [M1](docs/plan-m1.md), [M2](docs/plan-m2.md) and
[M3](docs/plan-m3.md) plans for how each was built.

- **M1:** ingest edge, attribution consumer, dead-letter handling, spike test proving zero
  loss and zero duplicates
- **M2:** AI inference consumer with backpressure, so inference latency stays flat under load
- **M3:** email channel and schema contract; a silent-failure monitor that emails alerts, with
  replay recovery; a crash test (23 `kill -9`s, data identical); an on-demand AWS environment
  and server benchmarks; the [event stream decision](docs/decisions/0001-event-stream.md) and a
  [postmortem](docs/postmortems/2026-11-14-silent-write-failure.md). To show it live, follow
  the [10-minute demo](docs/demo.md)
- **M4 (candidate):** batched and parallel attribution; priority lanes with the transactional
  outbox

## Running locally

See [running locally](docs/running.md): setup, the six services, trying it, the silent-failure
drill and the tests.

## Why this project

A learning project for event-driven architecture and current best practices for absorbing
webhook spikes: durable streams, at-least-once delivery, idempotency, and backpressure.
