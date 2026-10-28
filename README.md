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

Design phase. See the [design spec](docs/design.md) for the architecture, decisions and
milestones:

- **M1:** ingest edge, attribution consumer, dead-letter handling, spike test proving zero
  loss and zero duplicates
- **M2:** AI inference consumer with backpressure, so inference latency stays flat under load

## Why this project

A learning project for event-driven architecture and current best practices for absorbing
webhook spikes: durable streams, at-least-once delivery, idempotency, and backpressure.
