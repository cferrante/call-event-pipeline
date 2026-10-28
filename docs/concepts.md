# Concepts guide

New to NATS, JetStream or event-driven systems? Read [Life of a lead](#life-of-a-lead) first,
then [Life of an insight](#life-of-an-insight) for the AI side, and use the
[glossary](#glossary) to look things up. The [design spec](design.md) explains
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

## Life of an insight

The same webhook also gets an AI classification (milestone 2). This path runs alongside
attribution, at its own pace.

**1. The same stored event.** Nothing new is published: the event already in the `LEADS`
stream is read a second time, by a different [consumer](#consumer), `inference`, with its own
progress. A slow model therefore never holds back attribution, and vice versa: a
[bulkhead](#bulkhead).

**2. A fixed number of workers.** The inference service runs a [worker pool](#worker-pool) of
4, matching the model's [capacity](#capacity): the number of calls it can serve at full speed.
JetStream backs this up with [MaxAckPending](#maxackpending) = 4, so it never hands out more
than 4 unfinished messages, however many inference processes run.

**3. Skip what's already done.** A worker first checks whether this interaction already has an
insight. If so (a [redelivery](#redelivery)), it acks without calling the model, so repeats
don't spend model capacity.

**4. Call the model.** The worker sends the SMS text to the [inference](#inference) model and
times only that call: [model latency](#model-latency). In milestone 2 the model is a stub that
behaves like a real one, slowing down when given more calls than its capacity and timing out
when badly overloaded.

**5. Store the insight.** The result (intent, score, model version, model latency) goes into
the `insights` table, once per interaction. The time from the webhook arriving to this moment
is [time to insight](#time-to-insight).

**6. During a spike.** Webhooks arrive far faster than the model can classify them. The extra
messages wait in the stream ([consumer lag](#consumer-lag)), so time to insight rises, but the
model still only ever sees 4 calls at once, so model latency stays flat. Once the spike passes,
the backlog drains.

**7. Without the limit.** The spike test's [control experiment](#control-experiment) removes the
limit (200 workers). The model is overloaded, calls time out, retries spend capacity on calls
that fail again, and the backlog takes *longer* to drain despite 50 times the workers:
[congestion collapse](#congestion-collapse). Measured results are in
[benchmarks](benchmarks.md).

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

### Bulkhead

Splitting a system into compartments so a failure or overload in one can't sink the others,
named after the watertight sections of a ship's hull.
*In this project:* attribution and inference are separate [consumers](#consumer) with separate
progress, so a slow model can't hold back attribution. They still share Postgres and the CPU,
which is why attribution drains more slowly under load with inference running (see
[benchmarks](benchmarks.md)).

### Capacity

How much work something can do at once at full speed. Beyond it, work either waits or slows
everything down.
*In this project:* the stub model serves 4 calls at once; each call beyond that makes all of them
slower.

### Congestion collapse

When an overloaded system gets *less* useful work done the more it's given, because effort goes
into work that fails and is retried.
*In this project:* in the spike test's [control experiment](#control-experiment), 200 workers
overloaded the model; timed-out calls were retried, and inference took twice as long to drain
as with 4 workers.

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

### Control experiment

A run with the thing being tested taken away, to check it's actually what makes the
difference. If the control looks the same as the real run, the experiment can't tell anything.
*In this project:* `MODE=control ./loadtest/run.sh` removes the inference worker limit; the
model must visibly overload, or the test fails.

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

### Dual write

Updating two separate systems, such as saving to a database and then publishing a message, as
if it were one step. Nothing makes them succeed or fail together, so a crash between the two
leaves them disagreeing: the row is saved but the event is never published, or the reverse.
*In this project:* a reason priority lanes wait until M3; the safe way to do it is the
[transactional outbox](#transactional-outbox).

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

### Inference

Running a trained AI model on an input to get a prediction, as opposed to training it.
*In this project:* classifying each interaction's intent (sales, support, unknown). Milestone 2
uses a stub model that behaves like a real one under load; a real model could replace it.

### JetStream

The persistence layer built into the NATS server. It adds [streams](#stream) (stored messages),
[consumers](#consumer) (tracked progress) and [acks](#ack) to plain [NATS](#nats).
*In this project:* enabled with `nats-server -js`.

### k6

A free, open-source load-testing tool. Test scripts are written in JavaScript; k6 itself is a
single Go binary.
*In this project:* `loadtest/spike.js`, run by `loadtest/run.sh`.

### MaxAckPending

A consumer setting: the most messages JetStream will have handed out without an [ack](#ack).
When it's reached, JetStream sends nothing more until one is acked. It's enforced on the server,
so it holds however many workers or processes there are.
*In this project:* `inference` uses 4, the model's [capacity](#capacity).

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

### Model latency

How long one call to the AI model takes, from sending the input to receiving the result. It
measures the model alone, not time spent waiting in a queue beforehand; compare
[time to insight](#time-to-insight).
*In this project (M2):* the metric that must stay flat during a spike, because the worker pool
never sends the model more calls at once than it can handle. See the
[M2 plan](plan-m2.md#d5-what-to-measure).

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

### Time to insight

How long from a webhook arriving to its AI result being stored: the time spent waiting in the
stream ([consumer lag](#consumer-lag)) plus [model latency](#model-latency).
*In this project (M2):* expected to rise during a spike, while work waits its turn, and to
recover afterwards. That's the design working: the wait happens in the queue, not inside an
overloaded model. See the [M2 plan](plan-m2.md#d5-what-to-measure).

### Tracking number

A phone number assigned to one marketing campaign, so a call to it shows which campaign
produced it.
*In this project:* the `tracking_numbers` table.

### Transactional outbox

A pattern that solves the [dual write](#dual-write) problem. Instead of publishing a message
directly, the program writes it to an `outbox` table **in the same database transaction** as
the change it describes, so both are saved or neither is. A separate relay then reads the
outbox and publishes each message, marking it sent. If the relay crashes, it publishes again
on restart, so delivery is [at-least-once](#delivery-guarantees) and consumers must be
[idempotent](#idempotency).
*In this project:* planned for M3, so attribution can safely publish "first touch" and "repeat"
events to separate inference lanes.

### TwiML

Twilio's XML instructions telling it what to do next, returned in the webhook response, e.g.
`<Response><Dial>…</Dial></Response>` to forward a call.

### Virtual user

A simulated client in a load test that runs the test script in a loop. Often shortened to
**VU**. With an [arrival rate](#arrival-rate), k6 adds VUs as needed to keep the rate up.

### Webhook

An HTTP request a service (here Twilio) sends to your server when something happens.

### Worker pool

A fixed number of workers that take jobs from a queue, so no more than that many run at once.
*In this project:* the inference service runs 4 workers, matching the model's
[capacity](#capacity); attribution runs 1.

### X-Twilio-Signature

A header on every Twilio webhook: an HMAC-SHA1 of the URL and parameters, keyed with your auth
token. Checking it proves the request came from Twilio and wasn't altered.
*In this project:* `internal/twilio`.
