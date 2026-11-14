# Concepts guide

New to NATS, JetStream or event-driven systems? Read [Life of a lead](#life-of-a-lead) first,
then [Life of an insight](#life-of-an-insight) for the AI side and
[Life of an incident](#life-of-an-incident) for what happens when something breaks, and use the
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
[bulkhead](#bulkhead). Unlike attribution, it starts at messages published after it was
created (its [deliver policy](#deliver-policy)), so its first start doesn't classify a week of
old leads.

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

## Life of an incident

What happens when part of the pipeline breaks without crashing (milestone 3). This is the drill
in the README, played out step by step.

**1. A silent failure.** Attribution keeps running, passes its health check and keeps up with
the stream, but a bug means it [acks](#ack) messages without writing them. Nothing errors,
[consumer lag](#consumer-lag) stays at zero, and leads quietly go missing: a
[silent failure](#silent-failure). In drills this bug is switched on deliberately with
[fault injection](#fault-injection).

**2. Detection.** The monitor reads the pipeline every 5 seconds from outside. Its
*acked but not written* rule compares how far attribution has acked with how many rows it wrote
over the last minute; when rows fall below 90% of acks, the alert starts
[firing](#alert-firing-and-resolved). The [canary](#canary) is a second line of defence: its own
test webhook stops showing up in the database, so *arrivals stopped* fires 30 seconds later.

**3. Alerting.** The alert is logged, shown on the status page, and emailed once (not every 5
seconds). Email goes through a queue, so if the mail service is slow or
[rate-limited](#rate-limit-429) the monitor keeps checking, and the queue
[retries with backoff](#retry-with-backoff-and-jitter).

**4. Fix the cause.** Someone fixes the bug and restarts attribution, or, in a drill, switches
the fault off. New messages are written again and the alert resolves, but the rows lost during
the incident are still missing.

**5. Replay.** Every event is still in the stream (it keeps 7 days), so the fix for the lost
rows is a [replay](#replay): read the incident's period again and record whatever is missing.
Writes are [idempotent](#idempotency), so rows that were written are left alone.

**6. Proof.** The drill's test compares the database with a clean run of the same events using
a checksum: after replay they match exactly, and a second replay changes nothing.

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

### Alert (firing and resolved)

An alert has two moments: when it starts (*firing*) and when its condition clears (*resolved*).
Notifying on those two changes, not on every check, means one email per incident instead of one
every few seconds.
*In this project:* `internal/monitor`'s tracker turns each 5-second evaluation into firing and
resolved events; the email and the status page use them.

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

### Build tag

A label in a Go file's first line (`//go:build faultinject`) that leaves the file out of the
build unless the tag is asked for (`go build -tags faultinject`).
*Gotcha:* ordinary `go build`, `go test` and `go vet` never compile a tagged file, so it can
stop compiling unnoticed when code it uses changes ("rot"). CI therefore also vets the tagged
build.
*In this project:* the [fault injection](#fault-injection) code exists only in `faultinject`
builds, and a test checks that a normal binary doesn't contain it.

### Bulkhead

Splitting a system into compartments so a failure or overload in one can't sink the others,
named after the watertight sections of a ship's hull.
*In this project:* attribution and inference are separate [consumers](#consumer) with separate
progress, so a slow model can't hold back attribution. They still share Postgres and the CPU,
which is why attribution drains more slowly under load with inference running (see
[benchmarks](benchmarks.md)).

### Canary

A small, harmless piece of fake traffic sent all the time, so that if it stops arriving
somewhere is broken. Named after the canaries miners took underground as an early warning.
*In this project:* the monitor sends a signed voice webhook every 10 seconds from `canary` and
checks its row reaches Postgres; if none arrives for 30 seconds, *arrivals stopped* fires.
Reports leave canary rows out.

### Capacity

How much work something can do at once at full speed. Beyond it, work either waits or slows
everything down.
*In this project:* the stub model serves 4 calls at once; each call beyond that makes all of them
slower.

### Chaos test

Breaking a running system on purpose, at random, to prove it recovers without losing or
corrupting data, instead of assuming it does.
*In this project:* `loadtest/crash.sh` `kill -9`s a random service every 5–15 s under load, at
least 20 times, and checks the data against a [test oracle](#test-oracle); `KILLS=0` is the
clean run it must match. See [design section 5.5](design.md#55-delivery-guarantees).

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

### Contract test

A test that checks one side of an agreement between two programs against a shared, published
description of it, so each side can change independently and still be checked against the other.
*In this project:* the [schema contract](#schema-contract): the producer's test checks what it
publishes, and each consumer's test processes every published example.

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

### CPU profile

A recording of where a program spends its CPU time, taken by sampling which function is running
many times a second. It shows what to optimise, or that there is nothing worth optimising.
*In this project:* Go's `pprof`, switched on with `PPROF_ADDR` (loopback only). On the server our
own code was 0.1–0.5% of the services' CPU, which is why
[decision record 0001](decisions/0001-event-stream.md#the-c-question) rejects a rewrite in C++.

### Dead-letter queue

Where messages go when the system has given up processing them, together with the reason, so a
person can fix the cause and replay or discard them. Often shortened to **DLQ**.
*In this project:* stream `LEADS_DLQ`, subject `leads.dlq`, kept 30 days. Each dead letter has
the original bytes plus `Dlq-Reason` and `Dlq-Subject` headers. A healthy DLQ is empty.

### Deliberate break

Breaking the code on purpose to check a test catches it. A test that passes on its first run
might be testing nothing, and this is how to find out. Doing it systematically with tools is
called *mutation testing*.
*In this project:* every step does it, and it has found tests that couldn't fail: an ordering
test that passed with ordering broken, and a symbol check that looked for the wrong name
([M3 plan](plan-m3.md#deviations-from-the-plan)).

### Deliver policy

Where a new [consumer](#consumer) starts reading a [stream](#stream): from the first stored
message ("all"), from messages published after it's created ("new"), or from a given time or
position. It's fixed when the consumer is created; to change it, the consumer must be deleted
and recreated, losing its progress.
*In this project:* attribution uses "all", since every lead must be recorded; inference uses
"new", so a first start doesn't spend model capacity on old leads. `go run ./cmd/inference
-reset-consumer` recreates the inference consumer.

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

### Fault injection

Causing a failure on purpose, in a controlled way, to check the system detects it and recovers.
Practising this is a *drill*.
*In this project:* in a `faultinject` [build](#build-tag), `SIGUSR1` makes attribution ack
without writing; see [Life of an incident](#life-of-an-incident).

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

### kill -9

Sends `SIGKILL`, which ends a process at once: no cleanup and no chance to finish in-flight
work. Ctrl-C (`SIGINT`) or `SIGTERM` instead asks the process to stop, which is a
[graceful shutdown](#drain). A crash or power cut behaves like `kill -9`, so it is the honest
test of "nothing is lost".
*In this project:* `loadtest/crash.sh` kills a random service with `kill -9` every 5–15
seconds under load, and checks the data is exactly right afterwards.

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

### Rate limit (429)

A service's limit on how many requests a client may make; over it, the answer is HTTP `429 Too
Many Requests`, often with a header saying when to try again.
*In this project:* SendGrid's Mail Send API sends `X-RateLimit-Reset`; the alert queue waits
until then before retrying.

### Reconciliation

Comparing two records of the same events and counting the differences, to find work that was
lost or done twice without trusting either side's own report.
*In this project:* the monitor's *acked but not written* rule reconciles what attribution
acknowledged to the stream against rows written to Postgres; the crash and drill scripts
reconcile the database against the webhooks sent.

### Redelivery

JetStream sending a message again, after a [nak](#nak) or when [AckWait](#ackwait) expires.

### Replay

Reading a stream's stored messages again from a past point in time.
*In this project:* `attribution -replay-from <time>` reads from that time to the stream's newest
message through a temporary consumer, records what's missing, then deletes the consumer. The
[durable consumer](#durable-consumer) doesn't move, so the service keeps running.

### Retry with backoff and jitter

Trying a failed request again after waiting, with the wait growing each time (*backoff*:
0.25 s, 0.5 s, 1 s …) and a random variation (*jitter*) so many clients don't all retry at the
same instant and overload the service again. Only worth doing when the failure may pass: a
`429` or `5xx`, not a `400`.
*In this project:* the alert queue (1 s doubling to 60 s), the crash test's k6 sender, and
replay's writes. Compare JetStream's [BackOff](#backoff) setting, which is a different thing.

### Schema contract

A published, versioned description of an event's shape that producers and consumers agree on,
with tests on both sides, so a breaking change fails a test instead of production.
*In this project:* JSON Schema files and examples in `schemas/lead-event/`; the producer test
checks every [envelope](#envelope) validates, and each consumer must process every published
example. See [design section 6.1](design.md#61-the-schema-as-a-contract-m3).

### Session Manager

An AWS service that opens a shell or forwards a port to an instance through an agent on the
instance that calls out to AWS, so the instance needs no open inbound port or SSH key.
*In this project:* `make demo-status` forwards the status page and alert inbox to localhost
through it; scripted commands use its sibling, Run Command. See the
[demo environment](../infra/demo/README.md).

### Silent failure

A failure that raises no error: the process stays up and looks healthy while its work is
wrong or missing. More dangerous than a crash, because nothing notices.
*In this project:* the monitor watches results (rows written, canary arrivals), not just
whether processes run; see [Life of an incident](#life-of-an-incident).

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

### Sync interval

How often JetStream forces its writes from the operating system's memory onto the disk. An
acknowledged message is safe if the process dies, but a power loss can lose writes since the
last sync.
*In this project:* the default is every 2 minutes. Syncing every write (`always`) capped the
edge at 300–500 webhooks/s on the server; replicating to three nodes is the production answer
([decision record 0001](decisions/0001-event-stream.md#durability-and-sync_interval)).

### Term

*Terminate*: a consumer tells JetStream "never deliver this message again," without claiming
success. The message stays in the stream.
*In this project:* used right after a message is copied to the
[dead-letter queue](#dead-letter-queue).

### Test oracle

The source of truth a test compares results against. It should be independent of the code
being tested, or the test only checks the code agrees with itself.
*In this project:* the crash test builds every webhook from its sequence number, so it can
compute in SQL exactly what the database must hold, and compare.

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

### Tolerant reader

A consumer that reads only the fields it needs and ignores ones it doesn't recognise, so a
producer can add fields without breaking it.
*In this project:* both consumers ignore unknown fields, and a published example with an
extra field (`withheld-caller-unknown-field.json`) proves it; an unknown *schema version* is
still rejected, because that could change what known fields mean.

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
