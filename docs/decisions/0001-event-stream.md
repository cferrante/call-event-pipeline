# Decision record 0001: the event stream

**Status:** accepted · **Date:** 2026-11-14 · **Prices as of:** 2026-11-14, AWS us-east-1, on
demand · **Revisit when:** one of the [triggers](#when-to-revisit) below happens

## Decision

**Keep NATS JetStream** as the event stream, and **don't rewrite any service in C++** (or
another faster language).

At this pipeline's load, every option handles the traffic; throughput doesn't decide this.
What decides it is how each one delivers and retries a single message, what it costs to run
and test, and how much application code each one needs. JetStream gives per-message
acknowledgement, delayed retry and duplicate detection in the broker, runs natively as one
binary with the tests, and costs one small instance per node. **Kinesis on demand is cheaper
to run than a three-node JetStream cluster** (about $60–80 against $165 a month) and needs no
operations, but it would move retries, dead-lettering and deduplication into our code and
can't run locally without Docker or a paid account. Kafka, self-managed or on MSK, costs more
and fits worse at this size.

The profile settles the C++ question: our own code is **0.1% to 0.5%** of the services' CPU.

## Context

- **What the stream must do** ([design section 5](../design.md#5-components)): acknowledge a
  webhook only once it is stored; let attribution and inference consume at their own pace;
  keep 7 days of events so a consumer can replay a gap
  ([section 8.3](../design.md#83-fault-injection-and-recovery-m3)); absorb provider retries;
  park messages that keep failing in a dead-letter stream.
- **The load measured on AWS** ([server benchmarks](../benchmarks.md#server-benchmarks-m3)):
  spikes of 1,000 webhooks/s with no loss; the edge sustains at least 4,000/s (the load
  generator was the limit); attribution drains about 620/s, limited by one Postgres
  transaction per message. Events are about 0.4 KB (voice and SMS), up to a few KB for email.
- **The volumes costed below:** 1 million and 100 million events a month (an average of 0.4/s
  and 39/s), sized for 1,000/s spikes, two consumers, 7 days' retention, and three copies
  across availability zones for every option except the single-node row.
- **The project's constraints:** free and open-source tools, native installs with no Docker,
  integration tests against real dependencies, and a $10/month AWS budget for demos.

## Options and cost

Monthly cost at 730 hours. Prices are from the AWS Price List API, queried on 2026-11-14 for
us-east-1 on demand ([sources](#sources)). Storage is 20 GB gp3 per NATS node and 50 GB per
Kafka broker.

| Option | Sizing | 1M events/month | 100M events/month | Fixed part |
|---|---|---|---|---|
| **NATS JetStream, 3 nodes** (replicas 3) | 3 × c7g.large ($0.0725/h) | **$164** | **$164** | all of it |
| NATS JetStream, 1 node (as measured) | 1 × c7g.large | $55 | $55 | all of it |
| Kafka on EC2 (KRaft, combined mode) | 3 × m7g.large ($0.0816/h) | $191 | $191 | all of it |
| MSK provisioned | 3 × kafka.m7g.large ($0.204/h) | $462 | $462 | all of it |
| MSK provisioned, smallest | 3 × kafka.t3.small ($0.0456/h) | $115 | $115 | all of it |
| MSK Serverless | $0.75 per cluster-hour, 6 partitions | $554 | $565 | $554 |
| **Kinesis on demand** | 2 streams (events, dead letters) at $0.04/h | **$59** | **$76** | $58 |
| Kinesis provisioned | 2 shards, 7-day retention | $51 | $53 | $51 |

What the table shows:

- **At these volumes the cost is almost all fixed.** Data charges are cents: 100 million
  0.4 KB events is about 48 GB in a month. The choice is between fixed costs and the work each
  option needs, not between per-GB prices.
- **MSK is 3–10× the price of the alternatives** for capacity this pipeline would use under
  1% of. MSK Serverless's cluster-hour alone is $548 a month. The t3.small brokers are
  burstable instances that AWS positions for development, not production.
- **Kinesis on demand is the cheapest managed option.** It bills each record rounded up to
  1 KB, which doubles the data charge for our small events, but that is still a few dollars.
  The newer On-demand Advantage mode bills a minimum of 25 MB/s across the account, far above
  this load. Provisioned Kinesis is cheaper still, but two shards accept only 2,000 records/s:
  a spike above that is throttled, and the edge would then fail webhooks it can accept today.
- **Not in the table:** the people cost of running brokers. A self-managed cluster (NATS or
  Kafka) needs upgrades, disk and certificate care, and monitoring; MSK needs less, Kinesis
  almost none. Kinesis consumers built on AWS's client library also keep their checkpoints in
  a DynamoDB table, a small extra charge.

## Delivery semantics

The table that decides it. "Our code" means what the application would have to build itself.

| Need | NATS JetStream | Kafka (classic consumer groups) | Kinesis |
|---|---|---|---|
| Acknowledge one message, retry it later | Built in: `Ack`, `NakWithDelay`, `MaxDeliver` per message | Offsets per partition: a failing message blocks its partition, or our code commits past it and retries it elsewhere | Checkpoints per shard: same as Kafka |
| Dead letters after N attempts | Delivery count on every message; we publish to `LEADS_DLQ` | Our code counts attempts and writes a dead-letter topic | Our code counts and writes a second stream |
| Absorb a provider's retry of the same webhook | Duplicate window on `Nats-Msg-Id` (10 min) | No: the idempotent producer only removes its own resends, not a new request with the same content | No deduplication |
| A bounded worker pool for the model | `MaxAckPending` = workers; any number of workers on one stream | Parallelism limited to partitions, unless our code tracks offsets per message | Limited to shards, same pattern |
| Replay from a time | Ordered consumer from a start time (step 11) | `offsetsForTimes` | `AT_TIMESTAMP` iterator |
| Ordering | Per stream; not needed here | Per partition | Per shard |
| Durability on acknowledgement | Replicas 3: stored on a majority before the ack | `acks=all`: on in-sync replicas | Replicated across 3 AZs before the ack |

**Kafka's queue mode doesn't change this yet.** Share groups (KIP-932, "queues for Kafka")
give Kafka per-message acknowledgement and are production-ready in Apache Kafka 4.2. But MSK
offers 4.2 only on Express brokers, where share groups aren't supported yet, and the
dead-letter part (KIP-1191) isn't released. Worth re-reading when MSK supports them.

**Duplicates are guarded twice either way.** The database's unique `provider_id` is the final
guard ([design section 5.5](../design.md#55-delivery-guarantees)), so losing the stream's
duplicate window would not create duplicate rows. Each provider retry would cost an extra
message through both consumers: a database lookup in inference, which checks for an existing
insight before calling the model, and a wasted model call only when the retry arrives before
the first insight is stored.

### Durability and `sync_interval`

The demo runs one NATS node, which syncs to disk every 2 minutes by default (verified in
`/varz`). A power loss of that machine could lose up to 2 minutes of acknowledged webhooks.
Syncing every write (`sync_interval: always`) closes that gap but **capped the edge at
300–500/s**, against at least 4,000/s by default ([measured](../benchmarks.md#server-benchmarks-m3)).

**Decision: keep the default, and in production run three nodes with replicas 3.** A message
is then acknowledged once a majority of nodes in different availability zones hold it, so
losing one machine's power loses nothing; this is the same model as Kafka's `acks=all` and
Kinesis's replication. Syncing every write is for a single node that must not lose data at
any cost, at a tenth of the throughput. **Assumed, not measured:** the edge latency of a
three-node cluster, which adds a network round trip within the region to each publish.

## The C++ question

Would rewriting hot paths in C++ (or Rust) make the pipeline faster? Not measurably.
From the CPU profiles taken during a 1,000/s spike on the server
([benchmarks](../benchmarks.md#server-benchmarks-m3)):

| | ingest | attribution |
|---|---|---|
| CPU used | 24% of one core | 21% of one core |
| Go runtime (scheduler, GC, memory) | 50% | 43% |
| Syscalls (network and disk I/O) | 27% | 32% |
| Standard library (HTTP, crypto, JSON) | 18% | 15% |
| NATS and pgx clients | 4% | 9% |
| **Our code** | **0.1%** | **0.5%** |

- A rewrite could at best remove the last row: 0.1–0.5% of a quarter of a core.
- The limits that matter are elsewhere. Attribution's ~620/s is each message **waiting** for
  its own Postgres commit (about 1.6 ms), with the CPU 85% idle; batching several messages per
  transaction is the fix, in any language ([M4 candidate](../plan-m3.md#decisions-agreed-before-starting)).
  The edge's limit above 4,000/s was the load generator, with the server at most 50% busy.
- **A broker written in C++** (Redpanda, which speaks Kafka's protocol) answers a different
  question, and the measurements don't call for it either: the stream was never the
  bottleneck. Its Community Edition is under a Business Source License, not an open-source
  one, which this project's constraints rule out.

## Consequences

- **Kept:** per-message retries and dead letters in the broker; one binary that runs natively
  in every test; $0 locally; replay from a time.
- **Accepted:** running the brokers ourselves in production (three nodes, upgrades, disks,
  monitoring), which a managed service would do for us. JetStream also has a smaller
  ecosystem than Kafka (fewer connectors and stream processors), which this pipeline doesn't
  use.
- **Licence risk, noted:** in 2025 NATS's main sponsor, Synadia, proposed moving the server to
  a Business Source License; the agreement of 1 May 2025 keeps NATS under Apache 2.0 within
  the CNCF.

## When to revisit

- **The team moves to AWS-managed services and won't run brokers.** Kinesis on demand is then
  the choice, at roughly $60–80 a month at these volumes; budget for moving retries,
  dead-lettering and deduplication into application code, and for integration tests against a
  real (paid) stream.
- **Sustained volume reaches tens of MB/s,** or the business needs Kafka's ecosystem
  (Connect, stream processing). Then Kafka, and on MSK once share groups are supported.
- **A profile shows our own code dominating CPU.** Only then is a faster language worth
  discussing.

## Sources

Accessed 2026-11-14.

- **Prices:** AWS Price List API (`aws pricing get-products`, region us-east-1, on-demand
  terms), service codes `AmazonMSK`, `AmazonKinesis`, `AmazonEC2`. Same method as
  `infra/bootstrap/aws/prices.sh`.
- **Kinesis on-demand capacity** (new streams start at 4 MB/s and 4,000 records/s, scaling to
  twice the previous peak): [Kinesis quotas and limits](https://docs.aws.amazon.com/streams/latest/dev/service-sizes-and-limits.html);
  [launch post](https://aws.amazon.com/blogs/aws/amazon-kinesis-data-streams-on-demand-stream-data-at-scale-without-managing-capacity).
- **Kinesis billing, 1 KB rounding per record:** [Kinesis Data Streams pricing](https://aws.amazon.com/kinesis/data-streams/pricing/).
- **Kinesis On-demand Advantage** (25 MB/s minimum): [What's new, November 2025](https://aws.amazon.com/about-aws/whats-new/2025/11/amazon-kinesis-data-streams-ondemand-advantage).
- **Kafka share groups:** [KIP-932 preview release notes](https://cwiki.apache.org/confluence/display/KAFKA/Queues+for+Kafka+%28KIP-932%29+-+Preview+Release+Notes);
  [Confluent: share consumer GA](https://www.confluent.io/blog/kafka-queue-semantics-share-consumer-ga/);
  [Thoughtworks Radar: Kafka share groups](https://thoughtworks.com/radar/platforms/kafka-share-groups) (KIP-1191 dead letters not yet available).
- **MSK versions and share groups:** [Supported Apache Kafka versions](https://docs.aws.amazon.com/msk/latest/developerguide/supported-kafka-versions.html);
  [Lambda: polling Kafka topics in queue mode](https://docs.aws.amazon.com/lambda/latest/dg/polling-kafka-queue.html).
- **NATS licence:** [CNCF and Synadia align on the future of NATS](https://www.cncf.io/announcements/2025/05/01/cncf-and-synadia-align-on-securing-the-future-of-the-nats-io-project/).
- **Redpanda licence:** [Redpanda licensing](https://docs.redpanda.com/current/get-started/licensing/overview).
- **Measurements:** [server benchmarks](../benchmarks.md#server-benchmarks-m3), [M3 plan](../plan-m3.md).
