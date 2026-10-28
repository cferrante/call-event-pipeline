#!/usr/bin/env bash
# Spike test, fully isolated from your dev data: a fresh leads_loadtest database and a separate
# NATS server (own ports, temporary storage). Starts both services, runs loadtest/spike.js,
# waits for the backlog to drain, then checks nothing was lost or duplicated.
#
# Usage: ./loadtest/run.sh            (defaults: RATE=1000 req/s, RAMP=10s, HOLD=60s)
#        RATE=200 HOLD=10s ./loadtest/run.sh
# Exits non-zero if any check fails. Results go to loadtest/results/<timestamp>/.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; source .env; set +a

RATE=${RATE:-1000} RAMP=${RAMP:-10s} HOLD=${HOLD:-60s} DUP_RATE=${DUP_RATE:-0.05}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-600}
NATS_PORT=4322 MON_PORT=8322 INGEST_PORT=8091
DB_NAME=leads_loadtest
DB_URL="${DATABASE_URL%/*}/${DB_NAME}"
RESULTS="loadtest/results/$(date +%Y%m%d-%H%M%S)"
WORK=$(mktemp -d)
mkdir -p "$RESULTS"
pids=()

cleanup() {
  # Reverse start order, so each service can drain while NATS is still up.
  for (( i=${#pids[@]}-1; i>=0; i-- )); do kill -INT "${pids[i]}" 2>/dev/null || true; done
  wait 2>/dev/null || true
  psql -X -q "$DATABASE_URL" -c "DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)" || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log() { echo "[$(date +%H:%M:%S)] $*"; }
wait_for() { # wait_for <description> <command...>
  local what=$1; shift
  for _ in $(seq 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  log "timed out waiting for $what"; exit 1
}
sql() { psql -X -A -t "$DB_URL" -c "$1"; }
# Reads one number from the NATS monitoring endpoint (python3 because jq isn't installed).
nats_stat() { # nats_stat consumer_pending|consumer_ack_pending|dlq_messages
  curl -s "http://127.0.0.1:${MON_PORT}/jsz?streams=true&consumers=true" | python3 -c '
import json, sys
which = sys.argv[1]
streams = {s["name"]: s for a in json.load(sys.stdin).get("account_details", []) for s in a.get("stream_detail", [])}
if which == "dlq_messages":
    print(streams.get("LEADS_DLQ", {}).get("state", {}).get("messages", 0)); sys.exit()
consumers = {c["name"]: c for c in streams.get("LEADS", {}).get("consumer_detail", [])}
c = consumers.get("attribution", {})
print(c.get("num_pending" if which == "consumer_pending" else "num_ack_pending", 0))' "$1"
}

for port in $NATS_PORT $MON_PORT $INGEST_PORT; do
  if ss -ltn | grep -qE ":${port}\b"; then log "port ${port} is in use; stop whatever uses it"; exit 1; fi
done

log "building services"
go build -o "$WORK/ingest" ./cmd/ingest
go build -o "$WORK/attribution" ./cmd/attribution

log "creating database ${DB_NAME}"
psql -X -q "$DATABASE_URL" -c "DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)" -c "CREATE DATABASE ${DB_NAME}"

log "starting NATS on ${NATS_PORT}"
nats-server -js -sd "$WORK/nats" -a 127.0.0.1 -p $NATS_PORT -m $MON_PORT >"$RESULTS/nats.log" 2>&1 &
pids+=($!)
wait_for "NATS" curl -sf "http://127.0.0.1:${MON_PORT}/healthz"

log "starting attribution"
DATABASE_URL="$DB_URL" NATS_URL="nats://127.0.0.1:${NATS_PORT}" "$WORK/attribution" >"$RESULTS/attribution.log" 2>&1 &
pids+=($!)
wait_for "migrations" bash -c "[ \"\$(psql -X -A -t '$DB_URL' -c \"SELECT count(*) FROM pg_tables WHERE tablename = 'interactions'\")\" = 1 ]"
sql "INSERT INTO tracking_numbers (phone_e164, campaign_id, source) VALUES
       ('+18005550001', 'search-fall', 'google'),
       ('+18005550002', 'morning-show', 'radio'),
       ('+18005550003', 'spring-flyer', 'print')" >/dev/null

TOKEN=$(openssl rand -hex 16)
PUBLIC_URL="http://127.0.0.1:${INGEST_PORT}"
log "starting ingest on ${INGEST_PORT}"
INGEST_PORT=$INGEST_PORT INGEST_PUBLIC_URL=$PUBLIC_URL TWILIO_AUTH_TOKEN=$TOKEN NATS_URL="nats://127.0.0.1:${NATS_PORT}" \
  "$WORK/ingest" >"$RESULTS/ingest.log" 2>&1 &
pids+=($!)
wait_for "ingest" curl -sf "${PUBLIC_URL}/healthz"

log "sampling consumer lag every second into lag.csv"
(
  echo "elapsed_s,num_pending,num_ack_pending"
  start=$(date +%s)
  while true; do
    echo "$(( $(date +%s) - start )),$(nats_stat consumer_pending),$(nats_stat consumer_ack_pending)"
    sleep 1
  done
) >"$RESULTS/lag.csv" &
sampler=$!

log "running k6: ramp to ${RATE} req/s over ${RAMP}, hold ${HOLD}, ${DUP_RATE} duplicates"
k6_status=0
k6 run --quiet -e TARGET="$PUBLIC_URL" -e PUBLIC_URL="$PUBLIC_URL" -e TOKEN="$TOKEN" \
  -e RATE="$RATE" -e RAMP="$RAMP" -e HOLD="$HOLD" -e DUP_RATE="$DUP_RATE" \
  -e SUMMARY_FILE="$RESULTS/summary.env" loadtest/spike.js || k6_status=$?
source "$RESULTS/summary.env"

log "k6 finished; waiting for the backlog to drain (up to ${DRAIN_TIMEOUT}s)"
drain_start=$(date +%s)
while :; do
  rows=$(sql "SELECT count(*) FROM interactions")
  if [ "$(nats_stat consumer_pending)" = 0 ] && [ "$(nats_stat consumer_ack_pending)" = 0 ] && [ "$rows" -ge "$UNIQUE_ACCEPTED" ]; then
    break
  fi
  if (( $(date +%s) - drain_start > DRAIN_TIMEOUT )); then log "drain timed out"; break; fi
  sleep 1
done
drain_s=$(( $(date +%s) - drain_start ))
sleep 2 # let the sampler record the drained state
kill "$sampler" 2>/dev/null || true

rows=$(sql "SELECT count(*) FROM interactions")
leads=$(sql "SELECT count(*) FROM leads")
unknown=$(sql "SELECT count(*) FROM interactions WHERE source = 'unknown'")
dlq=$(nats_stat dlq_messages)
max_lag=$(tail -n +2 "$RESULTS/lag.csv" | cut -d, -f2 | sort -n | tail -1)

failures=0
check() { # check <description> <condition...>
  if "${@:2}"; then printf '  PASS  %s\n' "$1"; else printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); fi
}
{
  echo "Spike test $(basename "$RESULTS"): ramp to ${RATE} req/s over ${RAMP}, hold ${HOLD}"
  echo
  echo "  requests sent           ${REQUESTS} (${DUPLICATES_SENT} deliberate duplicates)"
  echo "  unique webhooks accepted ${UNIQUE_ACCEPTED}"
  echo "  interactions in Postgres ${rows} (${unknown} to the unknown number, ${leads} leads)"
  echo "  edge latency ms          p50 ${P50_MS}  p95 ${P95_MS}  p99 ${P99_MS}  max ${MAX_MS}"
  echo "  failed requests          $(python3 -c "print(f'{${FAILED_RATE} * 100:.3f}%')")"
  echo "  peak consumer lag        ${max_lag} messages; drained ${drain_s}s after k6 finished"
  echo "  dead letters             ${dlq}"
  echo
  check "k6 thresholds met (0% failed, p99 < 50 ms)" test "$k6_status" -eq 0
  check "zero loss: every accepted webhook is in Postgres" test "$rows" -ge "$UNIQUE_ACCEPTED"
  check "zero duplicates: no extra rows" test "$rows" -le "$UNIQUE_ACCEPTED"
  check "nothing dead-lettered" test "$dlq" -eq 0
  check "backlog fully drained" test "$(nats_stat consumer_pending)" -eq 0
} >"$RESULTS/report.txt" # not "| tee": a pipeline would run check in a subshell and lose $failures
cat "$RESULTS/report.txt"

commit=$(git rev-parse --short HEAD)
[ -n "$(git status --porcelain)" ] && commit="${commit}+dirty"
machine="$(nproc) CPU, $(grep -qi microsoft /proc/version && echo 'Ubuntu WSL2' || uname -s)"
failed_pct=$(python3 -c "print(f'{${FAILED_RATE} * 100:.3f}%')")
match="$([ "$rows" -eq "$UNIQUE_ACCEPTED" ] && echo ✓ || echo ✗) ${rows}"
echo
echo "Row for docs/benchmarks.md:"
echo "| $(date +%F) | \`${commit}\` | ${machine} | ${RATE}/s, ramp ${RAMP}, hold ${HOLD} |" \
  "${P50_MS} / ${P99_MS} ms | ${failed_pct} | ${max_lag} | ${drain_s}s | ${match} | ${dlq} |"
echo

[ "$failures" -eq 0 ] && log "PASS ($RESULTS)" || { log "FAIL: ${failures} check(s) ($RESULTS)"; exit 1; }
