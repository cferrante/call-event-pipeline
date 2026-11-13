#!/usr/bin/env bash
# Crash test (docs/design.md, section 5.5): under steady load, kill -9 a random service
# (ingest, attribution, inference or nats-server) every 5-15 s while a restart loop brings it
# back, then check the database holds exactly what the webhooks say it should: no loss, no
# duplicates, every field right. Isolated like run.sh: its own database and NATS server.
#
# Every webhook is a pure function of its iteration number (loadtest/crash.js), so the expected
# contents are computed in SQL, independent of the pipeline. KILLS=0 runs the same load
# without kills: the clean run, which must give the same checksum.
#
# Usage: ./loadtest/crash.sh                   kills until the load ends (at least MIN_KILLS)
#        KILLS=0 ./loadtest/crash.sh           the clean run
#        RATE=100 DURATION=240s SEED=1 MIN_KILLS=20 ./loadtest/crash.sh
# Exits non-zero if any check fails. Results go to loadtest/results/<timestamp>-crash/.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; source .env; set +a

RATE=${RATE:-100} DURATION=${DURATION:-240s} SEED=${SEED:-1} MIN_KILLS=${MIN_KILLS:-20}
KILLS=${KILLS:-on} DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-300}
MODEL_LATENCY=${MODEL_LATENCY:-10ms} MODEL_TIMEOUT=${MODEL_TIMEOUT:-200ms} MODEL_CAPACITY=${MODEL_CAPACITY:-4}
NATS_PORT=4323 MON_PORT=8323 INGEST_PORT=8092
DB_NAME=leads_crash
DB_URL="${DATABASE_URL%/*}/${DB_NAME}"
NATS_URL_CT="nats://127.0.0.1:${NATS_PORT}"
MODE=$([ "$KILLS" = 0 ] && echo clean || echo crash)
RESULTS="loadtest/results/$(date +%Y%m%d-%H%M%S)-${MODE}"
WORK=$(mktemp -d)
mkdir -p "$RESULTS"
loops=()

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$RESULTS/events.log"; }
sql() { psql -X -A -t "$DB_URL" -c "$1"; }

cleanup() {
  touch "$WORK/stop"
  for name in ingest inference attribution nats; do
    [ -f "$WORK/$name.pid" ] && kill -INT "$(cat "$WORK/$name.pid")" 2>/dev/null || true
    sleep 0.5
  done
  for pid in "${loops[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  psql -X -q "$DATABASE_URL" -c "DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)" || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# supervise <name> <command...>: run the command, and start it again whenever it exits, until
# the test stops; like systemd's Restart=always on the server. The current pid is in
# $WORK/<name>.pid for the killer.
supervise() {
  local name=$1; shift
  (
    while [ ! -f "$WORK/stop" ]; do
      "$@" >>"$RESULTS/$name.log" 2>&1 &
      echo $! >"$WORK/$name.pid"
      wait $! || true
      [ -f "$WORK/stop" ] || { echo "[supervisor] $name exited; restarting" >>"$RESULTS/$name.log"; sleep 0.5; }
    done
  ) 2>>"$RESULTS/supervisor.log" & # bash's "Killed" notices go here, not to the terminal
  loops+=($!)
}
wait_for() {
  local what=$1; shift
  for _ in $(seq 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  log "timed out waiting for $what"; exit 1
}
# consumer_pending <name> prints its pending + ack-pending count, or "?" if NATS can't say.
consumer_pending() {
  curl -sf "http://127.0.0.1:${MON_PORT}/jsz?streams=true&consumers=true" 2>/dev/null | python3 -c '
import json, sys
name = sys.argv[1]
for a in json.load(sys.stdin).get("account_details", []):
    for s in a.get("stream_detail", []):
        if s["name"] == "LEADS":
            for c in s.get("consumer_detail", []):
                if c["name"] == name:
                    print(c["num_pending"] + c["num_ack_pending"]); sys.exit()
print("?")' "$1" 2>/dev/null || echo "?"
}
dead_letters() {
  curl -sf "http://127.0.0.1:${MON_PORT}/jsz?streams=true" 2>/dev/null | python3 -c '
import json, sys
print(next((s["state"]["messages"] for a in json.load(sys.stdin).get("account_details", [])
            for s in a.get("stream_detail", []) if s["name"] == "LEADS_DLQ"), 0))' 2>/dev/null || echo "?"
}

for port in $NATS_PORT $MON_PORT $INGEST_PORT; do
  if ss -ltn | grep -qE ":${port}\b"; then log "port ${port} is in use; stop whatever uses it"; exit 1; fi
done

log "building services"
for svc in ingest attribution inference; do go build -o "$WORK/$svc" "./cmd/$svc"; done
psql -X -q "$DATABASE_URL" -c "DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)" -c "CREATE DATABASE ${DB_NAME}"

log "starting nats-server, attribution, inference and ingest under restart loops"
supervise nats nats-server -js -sd "$WORK/nats" -a 127.0.0.1 -p $NATS_PORT -m $MON_PORT
wait_for "NATS" curl -sf "http://127.0.0.1:${MON_PORT}/healthz"
supervise attribution env DATABASE_URL="$DB_URL" NATS_URL="$NATS_URL_CT" "$WORK/attribution"
wait_for "migrations" bash -c "[ \"\$(psql -X -A -t '$DB_URL' -c 'SELECT count(*) FROM schema_migrations')\" = $(ls migrations/*.sql | wc -l) ]"
sql "INSERT INTO tracking_numbers (phone_e164, campaign_id, source) VALUES
       ('+18005550001', 'search-fall', 'google'),
       ('+18005550002', 'morning-show', 'radio'),
       ('+18005550003', 'spring-flyer', 'print')" >/dev/null
supervise inference env DATABASE_URL="$DB_URL" NATS_URL="$NATS_URL_CT" INFERENCE_WORKERS=$MODEL_CAPACITY \
  MODEL_CAPACITY=$MODEL_CAPACITY MODEL_LATENCY=$MODEL_LATENCY MODEL_TIMEOUT=$MODEL_TIMEOUT "$WORK/inference"
TOKEN=$(openssl rand -hex 16)
PUBLIC_URL="http://127.0.0.1:${INGEST_PORT}"
supervise ingest env INGEST_PORT=$INGEST_PORT INGEST_PUBLIC_URL=$PUBLIC_URL TWILIO_AUTH_TOKEN=$TOKEN \
  NATS_URL="$NATS_URL_CT" "$WORK/ingest"
wait_for "ingest" curl -sf "${PUBLIC_URL}/healthz"

# The killer: a seeded sequence, so a failing run can be repeated kill for kill.
if [ "$KILLS" != 0 ]; then
  (
    RANDOM=$SEED
    targets=(ingest attribution inference nats)
    n=0
    while [ ! -f "$WORK/load-done" ] || (( n < MIN_KILLS )); do
      sleep $(( 5 + RANDOM % 11 ))
      target=${targets[RANDOM % 4]}
      if pid=$(cat "$WORK/$target.pid" 2>/dev/null) && kill -9 "$pid" 2>/dev/null; then
        n=$((n + 1))
        echo "[$(date +%H:%M:%S)] kill -9 ${target} (#${n})" | tee -a "$RESULTS/events.log" >>"$RESULTS/kills.log"
      fi
    done
  ) &
  killer=$!
fi

log "running k6: ${RATE}/s for ${DURATION}, ${MODE}"
k6 run --quiet -e TARGET="$PUBLIC_URL" -e PUBLIC_URL="$PUBLIC_URL" -e TOKEN="$TOKEN" \
  -e RATE="$RATE" -e DURATION="$DURATION" -e SUMMARY_FILE="$RESULTS/summary.env" \
  loadtest/crash.js 2>"$RESULTS/k6.log" || true
source "$RESULTS/summary.env"
touch "$WORK/load-done"
if [ "$KILLS" != 0 ]; then
  log "load done; waiting for the killer to reach ${MIN_KILLS} kills"
  wait "$killer" || true
  # The last kill may have just happened: make sure every service is back.
  wait_for "ingest after the last kill" curl -sf "${PUBLIC_URL}/healthz"
fi
kills=$( [ -f "$RESULTS/kills.log" ] && wc -l <"$RESULTS/kills.log" || echo 0)

log "waiting for both consumers to drain (up to ${DRAIN_TIMEOUT}s)"
# Done when both consumers have nothing pending and the counts have held for 5 s: waiting for
# the expected count instead would wait out the timeout whenever something was lost.
drain_start=$(date +%s) steady=0 last=""
while :; do
  rows=$(sql "SELECT count(*) FROM interactions" 2>/dev/null || echo 0)
  insights=$(sql "SELECT count(*) FROM insights" 2>/dev/null || echo 0)
  ap=$(consumer_pending attribution) ip=$(consumer_pending inference)
  if [ "$ap" = 0 ] && [ "$ip" = 0 ] && [ "$rows $insights" = "$last" ]; then steady=$((steady + 1)); else steady=0; fi
  last="$rows $insights"
  (( steady >= 5 )) && break
  if (( $(date +%s) - drain_start > DRAIN_TIMEOUT )); then log "drain timed out"; break; fi
  sleep 1
done
drained_s=$(( $(date +%s) - drain_start ))

# What the database must hold: one row per webhook, computed from the iteration numbers the
# same way crash.js builds them.
expected="
  SELECT CASE WHEN i % 2 = 0 THEN 'CAcrash' ELSE 'SMcrash' END || lpad(i::text, 8, '0') AS provider_id,
         CASE WHEN i % 2 = 0 THEN 'voice' ELSE 'sms' END AS channel,
         '+1415' || lpad((i % 500)::text, 7, '0') AS from_number,
         (ARRAY['+18005550001', '+18005550002', '+18005550003', '+18005559999'])[i % 4 + 1] AS to_e164,
         CASE WHEN i % 2 = 1 AND i % 3 = 0 THEN 'sales' WHEN i % 2 = 1 AND i % 3 = 1 THEN 'support'
              ELSE 'unknown' END AS intent
  FROM generate_series(0, ${ITERATIONS} - 1) AS i"
# The checksum covers every key field, the campaign each interaction was credited to, the lead
# it was linked to, and its insight. Lead first/last touch is left out: retries during an
# outage legitimately change the order calls arrive in.
checksum() { # checksum <rows query with provider_id, channel, from_number, to_e164, campaign_id, source, lead, intent>
  sql "SELECT count(*) || ' ' || md5(coalesce(string_agg(concat_ws('|', provider_id, channel, from_number, to_e164,
         campaign_id, source, lead, intent), ';' ORDER BY provider_id), '')) FROM ($1) AS r"
}
read -r want_n want_sum <<<"$(checksum "
  SELECT e.provider_id, e.channel, e.from_number, e.to_e164, t.campaign_id, coalesce(t.source, 'unknown') AS source,
         e.from_number AS lead, e.intent
  FROM (${expected}) AS e LEFT JOIN tracking_numbers t ON t.phone_e164 = e.to_e164")"
read -r got_n got_sum <<<"$(checksum "
  SELECT i.provider_id, i.channel, i.from_number, i.to_e164, i.campaign_id, i.source, l.phone_e164 AS lead, s.intent
  FROM interactions i LEFT JOIN leads l ON l.id = i.lead_id LEFT JOIN insights s ON s.provider_id = i.provider_id")"
leads=$(sql "SELECT count(*) FROM leads")
want_leads=$(( ITERATIONS < 500 ? ITERATIONS : 500 ))
missing=$(sql "SELECT count(*) FROM (${expected}) e WHERE NOT EXISTS (SELECT 1 FROM interactions i WHERE i.provider_id = e.provider_id)")
unexpected=$(sql "SELECT count(*) FROM interactions i WHERE NOT EXISTS (SELECT 1 FROM (${expected}) e WHERE e.provider_id = i.provider_id)")
no_insight=$(sql "SELECT count(*) FROM interactions i WHERE NOT EXISTS (SELECT 1 FROM insights s WHERE s.provider_id = i.provider_id)")
dlq=$(dead_letters)
restarts=$(cat "$RESULTS"/*.log | grep -c '^\[supervisor\]' || true)

failures=0
check() {
  if "${@:2}"; then printf '  PASS  %s\n' "$1"; else printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); fi
}
{
  echo "Crash test $(basename "$RESULTS"): ${RATE}/s for ${DURATION}, seed ${SEED}, ${MODE}"
  echo
  echo "  webhooks                   ${ITERATIONS} (${ACCEPTED} accepted, ${GAVE_UP} gave up, ${DROPPED} not started)"
  echo "  retries by the sender      ${RETRIES}"
  echo "  kills / restarts           ${kills} / ${restarts}"
  echo "  drained                    ${drained_s}s after the load (including 5 s to confirm)"
  echo "  interactions               ${got_n} (expected ${want_n}; ${missing} missing, ${unexpected} unexpected)"
  echo "  without an insight         ${no_insight}"
  echo "  leads                      ${leads} (expected ${want_leads})"
  echo "  dead letters               ${dlq}"
  echo "  checksum                   ${got_sum} (expected ${want_sum})"
  echo
  [ "$MODE" = crash ] && check "at least ${MIN_KILLS} kills" test "$kills" -ge "$MIN_KILLS"
  check "every webhook accepted, none dropped" test "$((GAVE_UP + DROPPED))" -eq 0
  check "zero loss: no expected interaction missing" test "$missing" -eq 0
  check "zero duplicates: no interaction outside the expected set" test "$unexpected" -eq 0
  check "every interaction has an insight" test "$no_insight" -eq 0
  check "leads as expected" test "$leads" -eq "$want_leads"
  check "checksum identical to the expected data" test "$got_sum" = "$want_sum"
  check "nothing dead-lettered" test "$dlq" = 0
} >"$RESULTS/report.txt"
cat "$RESULTS/report.txt"

commit=$(git rev-parse --short HEAD)
[ -n "$(git status --porcelain)" ] && commit="${commit}+dirty"
machine="$(nproc) CPU, $(grep -qi microsoft /proc/version && echo 'Ubuntu WSL2' || uname -s)"
echo
echo "Row for docs/benchmarks.md:"
echo "| $(date +%F) | \`${commit}\` | ${machine} | ${MODE}, ${RATE}/s for ${DURATION}, seed ${SEED} | ${kills} | ${ITERATIONS} | ${RETRIES} |" \
  "${drained_s}s | $([ "$got_sum" = "$want_sum" ] && [ "$missing" = 0 ] && echo "✓ ${got_n}" || echo "✗ ${got_n}, ${missing} missing") | ${dlq} |"
echo
[ "$failures" -eq 0 ] && log "PASS ($RESULTS)" || { log "FAIL: ${failures} check(s) ($RESULTS)"; exit 1; }
