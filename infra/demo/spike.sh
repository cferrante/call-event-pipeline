#!/usr/bin/env bash
# make demo-spike: a webhook spike from the load generator to the system under test, then a
# check that every accepted webhook was stored once. Watch it live with `make demo-status`.
#   RATE=1000 HOLD=30s make demo-spike     (defaults; RAMP, IDLE, IDLE_RATE, DUP_RATE too)
# Each run uses its own CallSid prefix, so runs can be repeated. Exits non-zero if a check fails.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip)
[[ -n $sut && -n $loadgen ]] || die "no demo environment; run: make demo-up"
RATE=${RATE:-1000} HOLD=${HOLD:-30s} RAMP=${RAMP:-10s} IDLE=${IDLE:-10s} IDLE_RATE=${IDLE_RATE:-20}
DUP_RATE=${DUP_RATE:-0.05} DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-600}
prefix="CAd$(openssl rand -hex 3)"

# value KEY TEXT : KEY's value from KEY=value lines in TEXT.
value() { sed -n "s/^ *$1=//p" <<<"$2" | tail -1; }

read -r _ _ _ _ dlq_before <<<"$(remote "$sut" /opt/cep/stats.sh)"

act "spike: idle ${IDLE_RATE}/s for $IDLE, ramp to ${RATE}/s over $RAMP, hold $HOLD (run $prefix)"
# The backlog, every few seconds, while k6 runs: it rises during the spike and drains after.
(
  while sleep 4; do
    read -r ap _ ip _ _ <<<"$(remote "$sut" /opt/cep/stats.sh 2>/dev/null)" || continue
    printf '    backlog: attribution %6s   inference %6s\n' "$ap" "$ip"
  done
) &
sampler=$!
trap 'kill $sampler 2>/dev/null' EXIT
# The signing token reaches k6 through its environment, never its command line.
k6out=$(remote "$loadgen" "set -a; . /etc/cep/config.env; . /run/cep/env; set +a
export TARGET=http://$sut_ip:8081 PUBLIC_URL=\"\$INGEST_PUBLIC_URL\" TOKEN=\"\$TWILIO_AUTH_TOKEN\"
export RATE=$RATE HOLD=$HOLD RAMP=$RAMP IDLE=$IDLE IDLE_RATE=$IDLE_RATE DUP_RATE=$DUP_RATE PREFIX=$prefix
export SUMMARY_FILE=/tmp/summary.env
cd /tmp && /opt/cep/bin/k6 run --quiet /opt/cep/spike.js 2>/tmp/k6.err >/dev/null || true
cat /tmp/summary.env
echo UNEXPECTED=\$(grep -c 'unexpected response' /tmp/k6.err)") || die "k6 failed on the load generator"
accepted=$(value UNIQUE_ACCEPTED "$k6out") sent=$(value UNIQUE_SENT "$k6out")
[[ -n $accepted ]] || die "k6 printed no summary:"$'\n'"$k6out"

act "k6 done; waiting for both consumers to drain (up to ${DRAIN_TIMEOUT}s)"
res=$(remote "$sut" "/opt/cep/drain.sh $prefix $accepted $DRAIN_TIMEOUT $dlq_before") || die "drain check failed"
kill $sampler 2>/dev/null
rows=$(value ROWS "$res") insights=$(value INSIGHTS "$res") drained=$(value DRAINED_S "$res")

fails=0
check() { # check DESCRIPTION CONDITION...
  local what=$1; shift
  if "$@"; then ok "$what"; else todo "$what"; fails=$((fails + 1)); fi
}
echo
echo "Run $prefix: $(value REQUESTS "$k6out") requests at up to ${RATE}/s, $(value DUPLICATES_SENT "$k6out") deliberate duplicates"
echo "  edge latency (k6): p50 $(value P50_MS "$k6out") ms, p95 $(value P95_MS "$k6out") ms, p99 $(value P99_MS "$k6out") ms"
echo "  model latency:     p50 $(value MODEL_P50 "$res") ms, p95 $(value MODEL_P95 "$res") ms (stub at load-test speed)"
echo "  drained ${drained} s after the spike"
check "no failed requests ($(value FAILED_RATE "$k6out") failed, $(value UNEXPECTED "$k6out") unexpected)" \
  test "$(value FAILED_RATE "$k6out")" = 0 -a "$(value UNEXPECTED "$k6out")" = 0
check "nothing lost or duplicated: $rows calls stored for $accepted accepted ($sent sent)" \
  test "$rows" -ge "$accepted" -a "$rows" -le "$sent"
check "every call classified: $insights insights" test "$insights" -ge "$accepted" -a "$insights" -le "$sent"
check "no dead letters" test "$(value NEW_DEAD_LETTERS "$res")" = 0
check "drained within ${DRAIN_TIMEOUT}s" test "$drained" != none
(( fails == 0 ))
