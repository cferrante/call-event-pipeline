# Call event pipeline

A real-time pipeline that captures every inbound call, text and email lead, credits it to the
marketing campaign that produced it, and has an AI model sort sales inquiries from support
questions. It's built to lose nothing, even during a sudden spike in traffic.

## Why

When a TV spot airs or a campaign takes off, contacts arrive in a rush, and that's exactly when
a business can least afford to lose one. Lead systems tend to fail in one of two ways under a
rush: webhooks time out and leads vanish, or everything slows down, including the AI that
decides which contacts are sales leads.

This project shows a design that avoids both, and proves it with measurements rather than
claims.

## How it works

```
call, text or email webhook
        │
        ▼
  ingest edge ─── verifies the signature, stores the event, then answers 200
        │
        ▼
  NATS JetStream (durable stream) ───► dead-letter stream for events that can't be processed
        ├──► attribution ───► Postgres    which campaign produced this contact?
        └──► AI inference ──► Postgres    sales, support or neither?

  monitor ─── sends test webhooks, checks they're saved, emails an alert if not
```

Four ideas carry the design:

- **Acknowledge only what's safe.** The edge answers the provider only once the event is
  durably stored, in about a millisecond. All slower work happens afterwards.
- **Each consumer keeps its own pace.** Attribution and AI inference read the stream
  independently, so a slow model can't hold up attribution, and neither can hold up the edge.
  A spike becomes a short backlog in the stream instead of lost leads.
- **Protect the model.** Inference runs a fixed number of requests at a time, so the model's
  response time stays level under load and the backlog waits safely in the stream.
- **Count every contact once.** Delivery is at-least-once, so every write is idempotent: an
  event delivered twice is still stored once.

The call, SMS and email providers (Twilio and SendGrid) are simulated against their published
webhook contracts, including signature checks, so the system runs end to end without real
accounts. New to NATS or JetStream? The [concepts guide](docs/concepts.md) explains the terms.

## What's proven

Measured on AWS servers on 14 November 2026; the method and raw results are in the
[benchmarks](docs/benchmarks.md).

| Claim | Result |
|---|---|
| A spike loses nothing | Three spikes of 1,000 contacts a second: all 67,799 stored exactly once each time; the backlog cleared about 2 minutes after the spike ended |
| The edge stays fast | About 1 ms per webhook, and it kept up with at least 4,000 a second, the most the load generator could send |
| The AI keeps its pace | Model response time held within 4% during a spike; without the limit, the same spike made it 17 times slower |
| Crashes lose nothing | 23 forced kills (`kill -9`) under load; the data afterwards was identical to a run with no kills |
| Silent failures are caught | A consumer that kept running but stopped saving was detected and emailed within 5–7 seconds; replay restored every missed event |
| The AI is accurate | A hosted model labelled 47 of 48 synthetic test messages correctly (98%), against 54% for keyword rules |

The load tests use a stand-in model calibrated from a real one, so they cost nothing to run;
the accuracy figure comes from the real hosted model.

## Status

Milestones 1 to 3 are complete:

- **M1:** ingest edge, attribution, dead-letter handling, and a spike test proving zero loss
  and zero duplicates.
- **M2:** AI inference with a limit on concurrent requests, so model latency stays flat under
  load.
- **M3:** email channel and a versioned event schema; a silent-failure monitor with email
  alerts and replay recovery; the crash test; an on-demand AWS environment and server
  benchmarks.

Next candidates: batched attribution (today about 620 contacts a second, saved one at a time)
and a priority lane so urgent sales leads skip a backlog.

## Stack

Go · NATS JetStream · PostgreSQL · k6 for load testing · OpenTofu for the AWS environment.
Everything runs natively on Linux, without Docker, using free and open-source tools.

## Try it

Run it on your own machine with [running locally](docs/running.md): setup, the services, a few
signed test webhooks, the silent-failure drill and the tests.

A hosted live demo on AWS is planned. Until it's up, the [10-minute demo runbook](docs/demo.md)
walks through it step by step, with the expected result of each step.

## How it was built

Spec first, then a step-by-step plan, then code, written test-first with
[Claude Code](https://claude.com/claude-code) as the coding agent. Each milestone closed with a
code review, and its findings and outcomes are recorded in that milestone's plan. Integration tests run against real NATS and Postgres rather than mocks.

| Read this | For |
|---|---|
| [One-page summary](docs/pitch.md) | The problem, results and costs, for a non-technical reader |
| [Design spec](docs/design.md) | Architecture, event schema, data model and delivery guarantees |
| [Decision record](docs/decisions/0001-event-stream.md) | Why NATS JetStream over Kafka and Kinesis, with monthly costs |
| [Postmortem](docs/postmortems/2026-11-14-silent-write-failure.md) | A silent write failure: how it was detected and recovered |
| Plans for [M1](docs/plan-m1.md), [M2](docs/plan-m2.md), [M3](docs/plan-m3.md) | Each step, the test that had to fail first, deviations and review outcomes |
