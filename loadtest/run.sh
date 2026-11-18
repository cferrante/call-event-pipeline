#!/usr/bin/env bash
# Spike test, fully isolated from your dev data: a fresh leads_loadtest database and a separate
# NATS server (own ports, temporary storage). Starts ingest, attribution and inference, runs
# loadtest/spike.js (an idle period, then a spike), waits for both consumers to drain, then
# checks nothing was lost or duplicated and that the model's latency stayed flat.
#
# Usage: ./loadtest/run.sh                  normal: inference limited to the model's capacity
#        MODE=control ./loadtest/run.sh     control: the limit removed, so the model overloads
#        RATE=200 HOLD=10s ./loadtest/run.sh
# Durations are whole seconds ("10s"). Exits non-zero if any check fails. Results go to
# loadtest/results/<timestamp>/.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; source .env; set +a

MODE=${MODE:-normal}
RATE=${RATE:-1000} RAMP=${RAMP:-10s} HOLD=${HOLD:-60s} DUP_RATE=${DUP_RATE:-0.05}
IDLE=${IDLE:-20s} IDLE_RATE=${IDLE_RATE:-20}
# The model runs 31 times faster than the calibrated real one (309 ms), so a spike drains in
# minutes; what the test proves (flat model latency, no loss) does not depend on the scale.
MODEL_LATENCY=${MODEL_LATENCY:-10ms} MODEL_TIMEOUT=${MODEL_TIMEOUT:-200ms} MODEL_CAPACITY=${MODEL_CAPACITY:-4} MODEL_JITTER=${MODEL_JITTER:-0.1}
case "$MODE" in
  normal) INFERENCE_WORKERS=${INFERENCE_WORKERS:-$MODEL_CAPACITY}; DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-600} ;;
  control) INFERENCE_WORKERS=${INFERENCE_WORKERS:-200}; DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-900} ;;
  *) echo "MODE must be normal or control, got ${MODE}"; exit 1 ;;
esac
secs() { echo "${1%s}"; } # "10s" -> 10
NATS_PORT=4322 MON_PORT=8322 INGEST_PORT=8091
DB_NAME=leads_loadtest
DB_URL="${DATABASE_URL%/*}/${DB_NAME}" # passed to services in their environment only
source loadtest/pg.sh
pg_use "$DATABASE_URL"
NATS_URL_LT="nats://127.0.0.1:${NATS_PORT}"
RESULTS="loadtest/results/$(date +%Y%m%d-%H%M%S)-${MODE}"
WORK=$(mktemp -d)
mkdir -p "$RESULTS"
pids=()

cleanup() {
  # Reverse start order, so each service can drain while NATS is still up.
  for (( i=${#pids[@]}-1; i>=0; i-- )); do kill -INT "${pids[i]}" 2>/dev/null || true; done
  wait 2>/dev/null || true
  pg_drop_db "$DB_NAME" || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log() { echo "[$(date +%H:%M:%S)] $*"; }
wait_for() { # wait_for <description> <command...>
  local what=$1; shift
  for _ in $(seq 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  log "timed out waiting for $what"; exit 1
}
sql() { PGDATABASE=$DB_NAME psql -X -A -t -c "$1"; }
# Prints "attribution_pending attribution_ack_pending inference_pending inference_ack_pending
# dead_letters" from the NATS monitoring endpoint, in one call (python3: jq isn't installed).
# Retries a failed read; if it keeps failing, prints "?" for each, which no check accepts as 0.
nats_stats() {
  local out
  for _ in 1 2 3; do
    out=$(nats_stats_once 2>/dev/null) && [[ $out =~ ^[0-9]+( [0-9]+){4}$ ]] && { echo "$out"; return; }
    sleep 0.5
  done
  echo "? ? ? ? ?"
}
nats_stats_once() {
  curl -sf "http://127.0.0.1:${MON_PORT}/jsz?streams=true&consumers=true" | python3 -c '
import json, sys
streams = {s["name"]: s for a in json.load(sys.stdin).get("account_details", []) for s in a.get("stream_detail", [])}
consumers = {c["name"]: c for c in streams.get("LEADS", {}).get("consumer_detail", [])}
out = []
for name in ("attribution", "inference"):
    c = consumers.get(name, {})
    out += [c.get("num_pending", 0), c.get("num_ack_pending", 0)]
out.append(streams.get("LEADS_DLQ", {}).get("state", {}).get("messages", 0))
print(*out)'
}

for port in $NATS_PORT $MON_PORT $INGEST_PORT; do
  if ss -ltn | grep -qE ":${port}\b"; then log "port ${port} is in use; stop whatever uses it"; exit 1; fi
done

log "building services"
for svc in ingest attribution inference; do go build -o "$WORK/$svc" "./cmd/$svc"; done

log "creating database ${DB_NAME}"
pg_recreate_db "$DB_NAME"

log "starting NATS on ${NATS_PORT}"
nats-server -js -sd "$WORK/nats" -a 127.0.0.1 -p $NATS_PORT -m $MON_PORT >"$RESULTS/nats.log" 2>&1 &
pids+=($!)
wait_for "NATS" curl -sf "http://127.0.0.1:${MON_PORT}/healthz"

log "starting attribution"
DATABASE_URL="$DB_URL" NATS_URL="$NATS_URL_LT" "$WORK/attribution" >"$RESULTS/attribution.log" 2>&1 &
pids+=($!)
PGDATABASE=$DB_NAME wait_for "migrations" bash -c "[ \"\$(psql -X -A -t -c \"SELECT count(*) FROM pg_tables WHERE tablename = 'insights'\")\" = 1 ]"
sql "INSERT INTO tracking_numbers (phone_e164, campaign_id, source) VALUES
       ('+18005550001', 'search-fall', 'google'),
       ('+18005550002', 'morning-show', 'radio'),
       ('+18005550003', 'spring-flyer', 'print')" >/dev/null

log "starting inference: ${INFERENCE_WORKERS} workers; model capacity ${MODEL_CAPACITY}, ${MODEL_LATENCY}, timeout ${MODEL_TIMEOUT}"
DATABASE_URL="$DB_URL" NATS_URL="$NATS_URL_LT" INFERENCE_WORKERS=$INFERENCE_WORKERS \
  MODEL_CAPACITY=$MODEL_CAPACITY MODEL_LATENCY=$MODEL_LATENCY MODEL_TIMEOUT=$MODEL_TIMEOUT MODEL_JITTER=$MODEL_JITTER \
  "$WORK/inference" >"$RESULTS/inference.log" 2>&1 &
pids+=($!)

TOKEN=$(openssl rand -hex 16)
PUBLIC_URL="http://127.0.0.1:${INGEST_PORT}"
log "starting ingest on ${INGEST_PORT}"
INGEST_PORT=$INGEST_PORT INGEST_PUBLIC_URL=$PUBLIC_URL TWILIO_AUTH_TOKEN=$TOKEN NATS_URL="$NATS_URL_LT" \
  "$WORK/ingest" >"$RESULTS/ingest.log" 2>&1 &
pids+=($!)
wait_for "ingest" curl -sf "${PUBLIC_URL}/healthz"

log "sampling consumer lag every second into lag.csv"
(
  echo "elapsed_s,attribution_pending,inference_pending"
  start=$(date +%s)
  while true; do
    read -r ap _ ip _ _ <<<"$(nats_stats)"
    echo "$(( $(date +%s) - start )),${ap},${ip}"
    sleep 1
  done
) >"$RESULTS/lag.csv" &
sampler=$!

log "running k6: idle ${IDLE_RATE}/s for ${IDLE}, ramp to ${RATE}/s over ${RAMP}, hold ${HOLD}, ${DUP_RATE} duplicates"
k6_status=0
k6 run --quiet -e TARGET="$PUBLIC_URL" -e PUBLIC_URL="$PUBLIC_URL" -e TOKEN="$TOKEN" \
  -e RATE="$RATE" -e RAMP="$RAMP" -e HOLD="$HOLD" -e DUP_RATE="$DUP_RATE" \
  -e IDLE="$IDLE" -e IDLE_RATE="$IDLE_RATE" \
  -e SUMMARY_FILE="$RESULTS/summary.env" loadtest/spike.js 2>"$RESULTS/k6.log" || k6_status=$?
source "$RESULTS/summary.env"
unexpected=$(grep -c 'unexpected response' "$RESULTS/k6.log" || true)

log "k6 finished; waiting for both consumers to drain (up to ${DRAIN_TIMEOUT}s)"
drain_start=$(date +%s) attribution_drain_s="" inference_drain_s=""
# Normal mode expects every accepted webhook to get an insight; in control, some are dead-lettered.
want_insights=$([ "$MODE" = normal ] && echo "$UNIQUE_ACCEPTED" || echo 0)
while :; do
  read -r ap aap ip iap _ <<<"$(nats_stats)"
  rows=$(sql "SELECT count(*) FROM interactions") insights=$(sql "SELECT count(*) FROM insights")
  elapsed=$(( $(date +%s) - drain_start ))
  if [ -z "$attribution_drain_s" ] && [ "$ap" = 0 ] && [ "$aap" = 0 ] && [ "$rows" -ge "$UNIQUE_ACCEPTED" ]; then
    attribution_drain_s=$elapsed
  fi
  if [ -n "$attribution_drain_s" ] && [ "$ip" = 0 ] && [ "$iap" = 0 ] && [ "$insights" -ge "$want_insights" ]; then
    inference_drain_s=$elapsed
    break
  fi
  if (( elapsed > DRAIN_TIMEOUT )); then log "drain timed out"; break; fi
  sleep 1
done
drained() { [ -n "$1" ] && echo "drained ${1}s after k6 finished" || echo "did not drain within ${DRAIN_TIMEOUT}s"; }
sleep 2 # let the sampler record the drained state
kill "$sampler" 2>/dev/null || true

rows=$(sql "SELECT count(*) FROM interactions")
insights=$(sql "SELECT count(*) FROM insights")
leads=$(sql "SELECT count(*) FROM leads")
unknown=$(sql "SELECT count(*) FROM interactions WHERE source = 'unknown'")
read -r ap _ ip _ dlq <<<"$(nats_stats)"
max_lag=$(tail -n +2 "$RESULTS/lag.csv" | cut -d, -f2 | sort -n | tail -1)
max_inference_lag=$(tail -n +2 "$RESULTS/lag.csv" | cut -d, -f3 | sort -n | tail -1)
timeouts=$(grep -c 'model overloaded' "$RESULTS/inference.log" || true)

# Model latency and time to insight (ms) for the idle window (the first IDLE seconds) and the
# spike window (the hold), by when each webhook was received. The windows start at the earliest
# interaction, not insight: in control mode early messages may have no insight (dead-lettered),
# which would shift both windows. Attribution records every webhook.
idle_end=$(secs "$IDLE") spike_start=$(( $(secs "$IDLE") + $(secs "$RAMP") ))
spike_end=$(( spike_start + $(secs "$HOLD") ))
window() { # window idle|spike -> "count model_p50 model_p95 tti_p50 tti_p95" (0s if empty)
  local from to
  if [ "$1" = idle ]; then from=0 to=$idle_end; else from=$spike_start to=$spike_end; fi
  sql "WITH t AS (SELECT min(occurred_at) AS t0 FROM interactions),
            w AS (SELECT model_ms, extract(epoch FROM created_at - received_at) * 1000 AS tti
                  FROM insights, t
                  WHERE received_at >= t0 + make_interval(secs => $from)
                    AND received_at <  t0 + make_interval(secs => $to))
       SELECT count(*),
              coalesce(round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY model_ms)::numeric, 2), 0),
              coalesce(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY model_ms)::numeric, 2), 0),
              coalesce(round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY tti)), 0),
              coalesce(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY tti)), 0)
       FROM w" | tr '|' ' '
}
read -r idle_n idle_m50 idle_m95 idle_t50 idle_t95 <<<"$(window idle)"
read -r spike_n spike_m50 spike_m95 spike_t50 spike_t95 <<<"$(window spike)"
# Model latency has fractional milliseconds, so compare with awk rather than bash integers.
flat() { # spike p95 within +10% of idle p95
  awk -v s="$spike_m95" -v i="$idle_m95" -v sn="$spike_n" -v idn="$idle_n" 'BEGIN { exit !(sn > 0 && idn > 0 && s <= i * 1.1) }'
}
overloaded() { # timeouts, or spike p95 at least 2x idle p95
  awk -v s="$spike_m95" -v i="$idle_m95" -v t="$timeouts" 'BEGIN { exit !(t > 0 || (i > 0 && s >= 2 * i)) }'
}

failures=0
check() { # check <description> <condition...>
  if "${@:2}"; then printf '  PASS  %s\n' "$1"; else printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); fi
}
{
  echo "Spike test $(basename "$RESULTS"): idle ${IDLE_RATE}/s for ${IDLE}, then ${RATE}/s (ramp ${RAMP}, hold ${HOLD})"
  echo "Inference: ${INFERENCE_WORKERS} workers; model capacity ${MODEL_CAPACITY}, ${MODEL_LATENCY}, timeout ${MODEL_TIMEOUT}"
  echo
  echo "  requests sent             ${REQUESTS} (${DUPLICATES_SENT} deliberate duplicates)"
  echo "  unique webhooks sent       ${UNIQUE_SENT}, accepted ${UNIQUE_ACCEPTED}"
  echo "  edge latency ms            p50 ${P50_MS}  p95 ${P95_MS}  p99 ${P99_MS}  max ${MAX_MS}"
  echo "  failed requests            $(python3 -c "print(f'{${FAILED_RATE} * 100:.3f}%')") (${unexpected} logged in k6.log)"
  echo "  interactions               ${rows} (${unknown} to the unknown number, ${leads} leads)"
  echo "  attribution lag            peak ${max_lag}; $(drained "$attribution_drain_s")"
  echo "  insights                   ${insights}"
  echo "  inference lag              peak ${max_inference_lag}; $(drained "$inference_drain_s")"
  echo "  model latency ms           idle p50 ${idle_m50} p95 ${idle_m95} (${idle_n})  spike p50 ${spike_m50} p95 ${spike_m95} (${spike_n})"
  echo "  time to insight ms         idle p50 ${idle_t50} p95 ${idle_t95}  spike p50 ${spike_t50} p95 ${spike_t95}"
  echo "  model timeouts (retried)   ${timeouts}"
  echo "  dead letters               ${dlq}"
  echo
  check "k6 thresholds met (0% failed, p99 < 50 ms)" test "$k6_status" -eq 0
  check "zero loss: every accepted webhook is an interaction" test "$rows" -ge "$UNIQUE_ACCEPTED"
  # A webhook can be stored yet answered 503 (its timeout fired as the PubAck arrived), so rows
  # may exceed "accepted"; a real duplicate is a row beyond anything that was ever sent.
  check "zero duplicates: no more interactions than webhooks sent" test "$rows" -le "$UNIQUE_SENT"
  check "attribution backlog fully drained" test "$ap" = 0
  if [ "$MODE" = normal ]; then
    check "model latency flat: spike p95 within 10% of idle p95" flat
    check "zero loss: every accepted webhook has an insight" test "$insights" -ge "$UNIQUE_ACCEPTED"
    check "zero duplicates: no more insights than webhooks sent" test "$insights" -le "$UNIQUE_SENT"
    check "inference backlog fully drained" test "$ip" = 0
    check "nothing dead-lettered" test "$dlq" = 0
  else
    # Control: without the limit the model is expected to overload, losing work to timeouts
    # and dead letters. If it doesn't, the experiment can't tell the protection apart.
    check "model overloaded without the limit (timeouts, or spike p95 at least 2x idle)" overloaded
  fi
} >"$RESULTS/report.txt" # not "| tee": a pipeline would run check in a subshell and lose $failures
cat "$RESULTS/report.txt"

commit=$(git rev-parse --short HEAD)
[ -n "$(git status --porcelain)" ] && commit="${commit}+dirty"
machine="$(nproc) CPU, $(grep -qi microsoft /proc/version && echo 'Ubuntu WSL2' || uname -s)"
failed_pct=$(python3 -c "print(f'{${FAILED_RATE} * 100:.3f}%')")
match="$([ "$rows" -ge "$UNIQUE_ACCEPTED" ] && [ "$rows" -le "$UNIQUE_SENT" ] && echo ✓ || echo ✗) ${rows}"
echo
echo "Rows for docs/benchmarks.md:"
echo "| $(date +%F) | \`${commit}\` | ${machine} | ${RATE}/s, ramp ${RAMP}, hold ${HOLD} |" \
  "${P50_MS} / ${P99_MS} ms | ${failed_pct} | ${max_lag} | ${attribution_drain_s:-timeout}s | ${match} | ${dlq} |"
echo "| $(date +%F) | \`${commit}\` | ${MODE}, ${INFERENCE_WORKERS} workers | ${idle_m95} / ${spike_m95} ms |" \
  "${idle_t95} / ${spike_t95} ms | ${max_inference_lag} | ${inference_drain_s:-timeout}s | ${insights} | ${timeouts} | ${dlq} |"
echo

[ "$failures" -eq 0 ] && log "PASS ($RESULTS)" || { log "FAIL: ${failures} check(s) ($RESULTS)"; exit 1; }
