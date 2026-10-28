# Concepts guide

New to NATS, JetStream or event-driven systems? Read [Life of a lead](#life-of-a-lead) first,
then use the [glossary](#glossary) to look things up. The [design spec](design.md) explains
*why* the system is built this way; this page explains the words it uses.

## Life of a lead

**1. A call comes in.** Someone calls one of our [tracking numbers](#tracking-number). Twilio
answers and sends a [webhook](#webhook): an HTTP POST describing the call (`CallSid`, `From`,
`To`, …) to our ingest [edge](#edge).

**2. The edge checks and stores it.** The edge verifies the
[`X-Twilio-Signature`](#x-twilio-signature) header so forged requests are rejected, wraps the
call details in an [envelope](#envelope), and [publishes](#publish) it to the
[subject](#subject) `leads.raw.voice`.

**3. JetStream makes it durable.** [NATS](#nats) delivers messages between programs;
[JetStream](#jetstream) is the part that also *stores* them. Our [stream](#stream) `LEADS`
captures every subject matching `leads.raw.>` and writes the message to disk. It replies with
a [PubAck](#puback), and only then does the edge answer Twilio with `200` and some
[TwiML](#twiml). The [message ID](#message-id) is the `CallSid`, so if Twilio retries the
webhook, the stream's [duplicate window](#duplicate-window) drops the copy.

**4. The edge is done.** It never waits for anything slower. During a traffic spike, messages
pile up in the stream instead of slowing the edge down: that pile is
[consumer lag](#consumer-lag), and it drains once the spike passes. The
[consumer lag](#consumer-lag) entry shows a real measured curve.

**5. A worker picks it up.** The [consumer](#consumer) `attribution` is
[durable](#durable-consumer) (JetStream remembers its progress) and [pull-based](#pull-consumer)
(workers [fetch](#fetch) messages when they're ready). JetStream tracks the message as
[ack-pending](#pending-and-ack-pending) and starts its [AckWait](#ackwait) timer.

**6. The worker records it.** In one Postgres transaction it inserts the interaction (skipping
it if already there: this is [idempotency](#idempotency)), looks up which campaign the tracking
number belongs to, and updates the caller's [first and last touch](#first-and-last-touch).

**7a. Success → Ack.** The worker [acks](#ack) the message. JetStream marks it done for this
consumer.

**7b. A temporary failure → Nak.** If Postgres is down, the worker [naks](#nak) the message with
a delay, and JetStream [redelivers](#redelivery) it later. Each attempt raises the message's
[delivery count](#delivery-count).

**7c. A hopeless message → Dead letter.** If the message can never be processed (a
[poison message](#poison-message), e.g. corrupted JSON), or it has failed on its
[final attempt](#maxdeliver), the worker copies it to the [dead-letter queue](#dead-letter-queue)
and [terms](#term) it so it's never delivered again.

**Why duplicates are expected.** JetStream guarantees [at-least-once](#delivery-guarantees)
delivery: if a worker crashes after writing to Postgres but before acking, the message is
delivered again. That's safe because step 6 is idempotent. The full reasoning is in
[design section 5.5](design.md#55-delivery-guarantees).

## Glossary

Each entry: what it means, then how this project uses it.

### Ack

Short for *acknowledge*. A consumer tells JetStream "I've finished this message; don't send it
again."
*In this project:* the attribution worker acks only after its Postgres transaction commits.

### AckWait

How long JetStream waits for an [ack](#ack) before assuming the worker died and
[redelivering](#redelivery) the message.
*In this project:* 30 seconds. Each attempt is limited to 20 seconds so it fails on its own
before AckWait runs out.

### Advisory

A notification JetStream publishes about its own events, such as "a message reached
[MaxDeliver](#maxdeliver)", on subjects starting with `$JS.EVENT.ADVISORY.`.
*Gotcha:* advisories are ordinary NATS messages, not stored. If nobody is subscribed when one
is sent, it's gone. They can be captured by creating a stream on their subjects.
*In this project:* not used; see [MaxDeliver](#maxdeliver).

### Advisory lock

A Postgres lock on an arbitrary number rather than on a table or row; programs use it to take
turns.
*In this project:* the migrator holds one so two services starting at once don't apply the
same migration twice.

### Arrival rate

A way of generating load: start a fixed number of requests per second, whether or not earlier
ones have finished, like real traffic. The alternative, a fixed number of
[virtual users](#virtual-user) each waiting for a reply before sending again, lets a slow server
quietly reduce its own load; see [coordinated omission](#coordinated-omission).
*In this project:* the spike test uses k6's `ramping-arrival-rate` [executor](#executor).

### At-least-once

See [delivery guarantees](#delivery-guarantees).

### BackOff

A JetStream consumer setting listing redelivery delays (e.g. 1s, 5s, 30s).
*Gotcha:* when BackOff is set, JetStream uses its first value **instead of** [AckWait](#ackwait),
so any processing slower than that value is redelivered while still running.
*In this project:* deliberately not used; retry delays come from [NakWithDelay](#nak) instead.
See [design section 5.2](design.md#52-jetstream-configuration).

### Consumer

JetStream's record of one reader's progress through a [stream](#stream): which messages it has
been sent, which are [acked](#ack), and its settings (AckWait, MaxDeliver, …). Several worker
processes can share one consumer to split the work.
*In this project:* `attribution`, on stream `LEADS`. Milestone 2 adds `inference` with its own
progress, so slow AI work never holds back attribution.

### Consumer lag

How far a [consumer](#consumer) is behind the newest message: the number of messages stored
but not yet processed. JetStream reports it as `num_pending`.
*In this project:* the shock absorber. A spike makes lag rise, then drain; it's monitored at
`localhost:8222/jsz`. This is the baseline spike test, 1,000 webhooks/s for about 70 seconds
(numbers are messages waiting):

```
   0s      0
  10s    401
  20s   3780 ######
  30s   7210 ############
  50s  14865 ########################
  60s  18854 ###############################
  70s  22531 #####################################   spike ends (peak 22,675 at 71s)
  80s  16799 ###########################
  90s   9018 ###############
 100s    233
 101s      0                                         drained
```

The edge accepted every webhook in about a millisecond the whole time. More details are in
[benchmarks](benchmarks.md).

### Coordinated omission

A load-testing trap: if the test waits for each response before sending the next request, a
slow server receives fewer requests exactly when it's struggling, and the results look better
than reality. Twilio doesn't wait for you, so neither should the test.
*In this project:* avoided by using an [arrival rate](#arrival-rate).

### Dead-letter queue

Where messages go when the system has given up processing them, together with the reason, so a
person can fix the cause and replay or discard them. Often shortened to **DLQ**.
*In this project:* stream `LEADS_DLQ`, subject `leads.dlq`, kept 30 days. Each dead letter has
the original bytes plus `Dlq-Reason` and `Dlq-Subject` headers. A healthy DLQ is empty.

### Delivery count

How many times JetStream has sent a particular message to a consumer, available on each
message as `NumDelivered` in its metadata. 1 means first delivery.
*In this project:* chooses the retry delay, and identifies the [final attempt](#maxdeliver).

### Delivery guarantees

- **At-most-once:** each message is processed once or not at all; a crash can lose it.
- **At-least-once:** each message is processed once or more; never lost, sometimes repeated.
  JetStream with explicit acks gives this.
- **Exactly-once:** impossible to guarantee in general across two separate systems (here
  NATS and Postgres).
- **Effectively once:** at-least-once delivery plus [idempotent](#idempotency) processing, so
  repeats have no extra effect. This is what this project does.

### Drain

Stop receiving new messages, finish the ones already in progress, then close.
*In this project:* the attribution worker drains on shutdown so no message is half-processed.

### Duplicate window

How long a [stream](#stream) remembers [message IDs](#message-id); a message published with an
ID it has already seen in that window is dropped.
*In this project:* 10 minutes on `LEADS`. Older duplicates are caught by Postgres's unique
`provider_id`.

### Durable consumer

A [consumer](#consumer) with a name, whose progress JetStream keeps even when no worker is
connected. Restarting a worker resumes where it left off.
*In this project:* `attribution` is durable.

### E.164

The international phone number format: `+`, country code, number, no spaces, up to 15 digits.
For example `+14158675310`.
*In this project:* tracking numbers and identifiable callers are E.164. Withheld caller IDs
(e.g. `anonymous`) aren't.

### Edge

The part of a system at its outer boundary, receiving requests from the outside world.
*In this project:* the ingest service (`cmd/ingest`). It only verifies, stores and acknowledges
each webhook, never waiting for attribution or AI, so its latency stays flat during a spike.
"Edge latency" in the spike test is the time from k6 sending a webhook to receiving the 200,
which includes waiting for JetStream's [PubAck](#puback). Measured values are in
[benchmarks](benchmarks.md); see [percentile](#percentile) for p50 and p99.

### Envelope

Our name for the JSON event built from one webhook: channel, provider ID, from, to, receive
time and all original parameters.
*In this project:* `internal/envelope`; schema in [design section 6](design.md#6-event-schema).

### Executor

How [k6](#k6) schedules work in a scenario: for example, a fixed number of
[virtual users](#virtual-user), or a target [arrival rate](#arrival-rate).

### Fetch

A [pull consumer](#pull-consumer) asking JetStream for the next batch of messages.

### First and last touch

The campaign that first brought a caller in, and the one behind their most recent contact.
Chosen by when the calls happened, not the order they were processed.
*In this project:* columns on the `leads` table.

### Idempotency

An operation is idempotent if doing it twice has the same effect as doing it once.
*In this project:* recording an interaction is idempotent because its insert is skipped when the
`provider_id` already exists; see [design section 5.5](design.md#55-delivery-guarantees).

### JetStream

The persistence layer built into the NATS server. It adds [streams](#stream) (stored messages),
[consumers](#consumer) (tracked progress) and [acks](#ack) to plain [NATS](#nats).
*In this project:* enabled with `nats-server -js`.

### k6

A free, open-source load-testing tool. Test scripts are written in JavaScript; k6 itself is a
single Go binary.
*In this project:* `loadtest/spike.js`, run by `loadtest/run.sh`.

### MaxDeliver

The most times JetStream will deliver one message to a consumer. After that it stops
redelivering, but the message stays in the stream.
*In this project:* 6, meaning 5 attempts plus one spare. A message whose fifth attempt fails is
dead-lettered by the worker itself rather than via an [advisory](#advisory), so it's caught
even if no listener is running. The spare delivery covers a dead-letter write that fails, or a
worker that crashes, during that fifth attempt.

### Message ID

An ID a publisher attaches (header `Nats-Msg-Id`) so the [stream](#stream) can drop duplicates
within its [duplicate window](#duplicate-window).
*In this project:* the Twilio `CallSid` or `MessageSid`.

### Migration

A numbered SQL file that changes the database schema. Each is applied once, in order, and never
edited afterwards; changes go in a new file.
*In this project:* `migrations/`, applied by `go run ./cmd/migrate`.

### Nak

*Negative acknowledgement*: a consumer tells JetStream "I couldn't process this; send it again."
`NakWithDelay` adds "but not before this long."
*In this project:* used for temporary failures, with delays of 1s, 5s, then 30s.

### NATS

A lightweight messaging server: programs publish messages to [subjects](#subject) and others
subscribe. On its own ("core NATS") it delivers only to whoever is listening right now and
stores nothing. [JetStream](#jetstream) adds storage.

### Pending and ack-pending

Counters JetStream reports for a [consumer](#consumer):
- `num_pending`: stored messages not yet delivered ([consumer lag](#consumer-lag)).
- `num_ack_pending`: delivered but not yet acked (in progress).
- `num_redelivered`: redelivered messages that are *still* ack-pending.

*Gotcha:* `num_redelivered` drops back to 0 once those messages are acked, so it can't tell you
afterwards whether retries happened. Use the consumer's total deliveries instead.

### Percentile

The value below which a given share of measurements fall. **p50** (the median): half of requests
were faster. **p99**: 99% were faster, so 1 in 100 was slower. Tail percentiles (p95, p99) matter
more than the average, because a slow tail is what users and webhook senders actually hit.
*In this project:* the target is edge p99 under 50 ms at 1,000 req/s; measured values are in
[benchmarks](benchmarks.md).

### Poison message

A message that fails every time it's processed, e.g. corrupted data. Retrying it forever wastes
work; dropping it loses data. It belongs in the [dead-letter queue](#dead-letter-queue).

### PubAck

The reply a [stream](#stream) sends to a publisher once a message is stored. No PubAck (or an
error) means the message may not be stored.
*In this project:* the ingest edge answers Twilio only after the PubAck, so an acknowledged
webhook is never lost.

### Publish

Send a message to a [subject](#subject).

### Pull consumer

A [consumer](#consumer) whose workers ask for messages when they have capacity, rather than
having JetStream push messages at them. Pull is the natural fit for backpressure: a busy worker
simply asks less often.

### Redelivery

JetStream sending a message again, after a [nak](#nak) or when [AckWait](#ackwait) expires.

### Spike test

A load test that ramps traffic up sharply, holds it, and drops it again, to see how a system
copes with a sudden burst and how it recovers.
*In this project:* `./loadtest/run.sh`; results in [benchmarks](benchmarks.md).

### Stream

A named, stored sequence of messages captured from one or more [subjects](#subject), with limits
on how long or how much it keeps.
*In this project:* `LEADS` (subjects `leads.raw.>`, kept 7 days) and `LEADS_DLQ`.

### Subject

The address a NATS message is published to: dot-separated words like `leads.raw.sms`.
Wildcards: `*` matches exactly one word (`leads.raw.*`); `>` matches one or more words at the
end (`leads.>`).

### Term

*Terminate*: a consumer tells JetStream "never deliver this message again," without claiming
success. The message stays in the stream.
*In this project:* used right after a message is copied to the
[dead-letter queue](#dead-letter-queue).

### Throughput

How much work a system completes per unit of time, e.g. messages processed per second. Not the
same as latency: a system can answer each request quickly (low latency) while doing less total
work per second than is arriving (lower throughput), which is exactly when lag builds up.
*In this project:* in the spike test the edge accepts webhooks faster than the attribution
consumer processes them, so lag grows during the spike and drains afterwards; see
[benchmarks](benchmarks.md).

### Tracking number

A phone number assigned to one marketing campaign, so a call to it shows which campaign
produced it.
*In this project:* the `tracking_numbers` table.

### TwiML

Twilio's XML instructions telling it what to do next, returned in the webhook response, e.g.
`<Response><Dial>…</Dial></Response>` to forward a call.

### Virtual user

A simulated client in a load test that runs the test script in a loop. Often shortened to
**VU**. With an [arrival rate](#arrival-rate), k6 adds VUs as needed to keep the rate up.

### Webhook

An HTTP request a service (here Twilio) sends to your server when something happens.

### X-Twilio-Signature

A header on every Twilio webhook: an HMAC-SHA1 of the URL and parameters, keyed with your auth
token. Checking it proves the request came from Twilio and wasn't altered.
*In this project:* `internal/twilio`.
