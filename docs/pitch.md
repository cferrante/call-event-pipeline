# Every lead counted, even on the busiest day

**The problem.** When a TV spot airs or a campaign goes viral, calls, texts and emails arrive
in a rush. That is exactly when a business can least afford to lose one. Most lead systems
either buckle under the rush, letting leads time out and vanish, or slow everything down,
including the AI that sorts sales inquiries from support questions.

**What this pipeline does.** It takes every inbound call, text and email the moment the phone
or email provider reports it, stores it safely within about a millisecond, and only then does
the slower work: crediting it to the campaign that produced it, and having an AI model decide
whether it is a sales lead, a support request or neither. A rush becomes a short queue, worked
through in a couple of minutes, instead of lost leads or an overloaded AI.

## What we proved

Measured on Amazon Web Services servers on 14 November 2026, not on a laptop.

| Promise | Result |
|---|---|
| A sudden rush loses nothing | Three rushes of **1,000 contacts a second**: 67,799 contacts each time, every one stored exactly once. The queue cleared about **2 minutes** after the rush ended |
| The front door stays fast | Each contact was answered in about **1 millisecond**. It kept up to **at least 4,000 a second**, the most our test equipment could send |
| The AI keeps its pace | In the rush tests a stand-in for the AI, built to slow down under overload as a real model does, kept its response time level (within 4%). With our safety limit removed, the same rush made it **17 times slower** |
| Crashes lose nothing | We force-killed parts of the system **23 times** while contacts kept arriving. Afterwards the data was identical to a run with no crashes |
| Hidden failures are caught | When a part kept running but quietly stopped saving contacts, an alert email went out within **5–7 seconds** (the goal was one minute). Every missed contact was restored from the system's 7-day record, in seconds |
| The AI is accurate | A real hosted AI model sorted a test set of messages correctly **98%** of the time, against 54% for simple keyword rules |

## What it costs

- **To build and prove:** free, open-source software throughout. The server tests cost
  **$0.28**; a full demonstration session costs at most **$0.70**, and the servers switch
  themselves off after 3 hours so a forgotten session can't run up a bill.
- **To run:** the message store at the heart of it would cost about **$164 a month** on three
  servers for resilience. We compared it with the alternatives: Amazon Kinesis is cheaper
  (about $60–80 a month) but would mean writing and testing more of the safety features
  ourselves; Kafka, run ourselves or by Amazon, costs $190–$565 a month
  ([decision record](decisions/0001-event-stream.md)).

## What's next

- **Faster crediting:** crediting contacts to campaigns handles about 620 a second today,
  because it saves them one at a time. Saving them in batches is the next improvement.
- **Urgent contacts first:** a priority lane, so a hot sales lead isn't stuck behind a rush of
  routine messages.

The technical detail is in the [benchmarks](benchmarks.md), the
[incident write-up](postmortems/2026-11-14-silent-write-failure.md) and the
[design](design.md).
