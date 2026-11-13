# CLAUDE.md

This project is built with [Claude Code](https://claude.com/claude-code) using spec-driven,
test-driven development. This file gives each session the working agreement and the context it
needs, and doubles as a record of how the project is run for anyone reading the repo.

## What this is

A real-time ingestion layer for inbound voice and SMS leads. Webhooks are acknowledged as soon as
they are durably stored in a stream; independent consumers then attribute each contact to its
marketing campaign and classify it with an AI model, each at its own pace. The goal is to absorb
large webhook spikes with zero loss and without slowing down AI inference.

## Project state

Milestones 1 (ingest and attribution) and 2 (inference with backpressure) are complete.
Milestone 3 (schema contract and email channel, silent-failure monitor, crash test, on-demand
AWS environment and server benchmarks) is specified in [the M3 plan](docs/plan-m3.md). Priority
lanes with the transactional outbox are an M4 candidate.

This section lags behind the code. Check `cmd/`, `internal/`, `migrations/` and the latest plan
before concluding something does or doesn't exist.

## Where things live

- `docs/design.md` is the spec: architecture, event schema, data model, delivery guarantees, and
  [decisions and known limitations](docs/design.md#12-decisions-and-known-limitations).
- `docs/plan-m<N>.md` is the step-by-step plan for each milestone. Each step names the test that
  must fail first. Each plan also holds a **progress** table (step, PR, what it proves), the
  **deviations log**, **code-review outcomes** and **spike-test results**: each finished step,
  every change from the plan and every review outcome gets a row there, in the same PR.
- `docs/benchmarks.md` holds targets and measured results, with the method used.
- `docs/concepts.md` explains NATS, JetStream and the patterns used. Link to it rather than
  re-explaining a term.
- `README.md` is the entry point for running and trying the system.

## How we work

- **Spec, then plan, then code.** Agree a short design and a test list before writing code, and
  record any deviation from it in the plan.
- **Run autonomously; stop only for decisions.** Once a plan is agreed, work through its steps
  without pausing: test, commit, open and merge PRs. Diffs and commit messages don't need
  review. Stop when a decision needs the user (a deviation from the plan, a trade-off, a cost)
  or when there are steps for the user to run, and present them as numbered choices or steps.
- **Test-first.** Show the test failing for the right reason, make it pass, then break the code
  on purpose to prove the test catches it. Treat a test that passes on its first run with
  suspicion.
- **Real dependencies in integration tests.** They run against real NATS and Postgres, not
  mocks, because a mock can't catch a wrong SQL query or a misconfigured stream.
- **One logical change per commit and per PR**, tests green. Work on a branch; Claude opens and
  merges the PR.
- **Communication:** answer first, then the evidence (a table when comparing), then a
  recommendation and one clear question. Say what is verified and what is assumed; measure
  before claiming a number. When something fails, reproduce it, explain why the earlier tests
  missed it, then fix it test-first.
- **Hand-off steps:** end runnable work with numbered steps and the expected output of each.
  Every step that opens a new terminal starts with `set -a; source .env; set +a`.
- **Review pages:** a document that needs the user's review (spec, plan, decision record,
  postmortem, pitch) is published as a rendered review page before it is committed. Routine
  updates that follow from approved work, such as benchmark rows, don't need one.
- **Docs style:** reference sections with Markdown anchor links
  (`[section 7](docs/design.md#7-data-model-postgres)`), never the `§` sign.

## Commands

Services and tools run natively on Ubuntu (WSL2): Go, `nats-server`, PostgreSQL and k6. There is
no Docker. Load the environment first in every terminal: `set -a; source .env; set +a`.

- `./scripts/db-setup.sh`: one-time setup. Creates `.env` with random secrets, the app role and
  the database.
- `nats-server -js -sd ./data/nats -m 8222`: message broker; Ctrl-C to stop.
- `go run ./cmd/attribution`, `./cmd/inference`, `./cmd/ingest`, `./cmd/monitor`: the
  services, one per terminal. Each finishes in-flight work on Ctrl-C. The monitor's status page
  is on `http://127.0.0.1:8090` (loopback only).
- `go run ./cmd/mailsink`: stand-in for SendGrid's Mail Send API; receives the monitor's alert
  emails, listed at `http://127.0.0.1:8025/messages`.
- `go run ./cmd/sendhook`: send a signed fake webhook (`-channel sms`, `-bad-signature`).
- `go test ./...`: unit tests, no services needed.
- `go test -tags integration -count=1 ./...`: integration and end-to-end tests. `-count=1`
  matters, because Go's test cache can't see the services these tests depend on. Without `.env`
  loaded they are skipped yet reported as `ok`; use `-v` to see `SKIP`.
- `./loadtest/run.sh`: spike test, isolated from development data. `MODE=control` runs it
  without the inference limit, as the comparison.
- `./loadtest/crash.sh`: crash test, `kill -9` of random services under load with a restart
  loop; checks the data is exactly what the webhooks imply. `KILLS=0` is the clean run.
- `make demo-up` / `demo-down` / `demo-check`: the AWS demo environment
  ([infra/demo](infra/demo/README.md)); `make demo-test` runs its offline tests.
- `gofmt -l .` and `go vet ./...` before every commit.

## Architecture

```
webhook ─► cmd/ingest ─► JetStream stream LEADS ─┬─► cmd/attribution ─► Postgres
                                                 └─► cmd/inference ───► Postgres
                     permanent failures ─► LEADS_DLQ
```

- `cmd/ingest` verifies the Twilio signature, publishes and waits for the stream's
  acknowledgement, then returns 200. It never touches Postgres.
- Each consumer is a durable JetStream consumer with its own pace. A slow model cannot slow
  attribution, and neither can slow the edge. Delivery is at-least-once, so every consumer write
  is idempotent.
- `cmd/inference` protects the model with a bounded worker pool (`internal/consumer`), so model
  latency stays flat under a spike and the backlog waits in the stream instead.
- Libraries: `nats.go` (`jetstream`), `pgx/v5`, standard-library `log/slog`. All configuration
  comes from environment variables.

## Decisions already made

Don't re-propose these without new evidence; the reasoning is in the design spec and plans.

- **Go** for every service and CLI. **NATS JetStream** as the durable stream, **PostgreSQL** as
  the store; no Kafka, no Redis, no ORM, no web framework.
- **Native install, no Docker.** Free and open-source tools only.
- **Load tests use a stub model** that behaves like a real one under overload, calibrated from
  a real model (Groq free tier) measured at a low rate. Running load tests against a hosted
  model costs money per call, so it needs a cost estimate first.
- **Priority lanes are an M4 candidate**, because doing them safely requires the transactional
  outbox pattern.
- **SendGrid and Twilio are simulated** against their documented contracts; no real accounts.
- **No rewrite in another language** for speed without a CPU profile showing our own code, not
  I/O, dominates.
- **AWS:** the owner runs account-level bootstrap; Claude may run `make demo-up`/`demo-down`
  and server benchmarks within agreed plan steps. Every instance self-terminates after 3 hours.

## Security and privacy

- Every webhook has its signature verified and its body size capped.
- Phone numbers are PII: logs show only the last four digits.
- Secrets live only in the git-ignored `.env` (created by `db-setup.sh`) and are documented, with
  placeholder values, in `.env.example`. Never print them, put them on a command line or commit
  them.
- Before every push or PR, check the diff for personal information: email addresses, local or
  home paths, and credentials. Commits use the GitHub noreply address.

## Environment notes

- `date` on this machine is uutils, where `%3N` drops leading zeros. Use bash `$EPOCHREALTIME`
  or Go's clock for timings.
- The installed `gh` is old: `gh pr edit` fails (use `gh api -X PATCH`), and `gh pr checks` has
  no `--json`.

## Parallel sessions

A second Claude Code session works in its own git worktree on its own branch. Agree up front
which files each session owns (often the plan and `docs/benchmarks.md`), and stay out of the
other's.
