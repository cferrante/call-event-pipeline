#!/usr/bin/env bash
# make demo-spike: a webhook spike from the load generator to the system under test, then a
# check that every accepted webhook was stored once. Watch it live with `make demo-status`.
#   RATE=1000 HOLD=30s make demo-spike     (defaults; RAMP, IDLE, IDLE_RATE, DUP_RATE too)
#   MODE=control   inference with 200 workers against the model's capacity of 4: the model
#                  overloads, as in the M2 control run. Some messages may be dead-lettered.
#   PROFILE=1      30 s CPU profiles of ingest and attribution during the hold, saved to
#                  infra/demo/.build/profiles/ (needs HOLD of at least 40s)
# Each run uses its own CallSid prefix, so runs can be repeated. Exits non-zero if a check fails.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip)
[[ -n $sut && -n $loadgen ]] || die "no demo environment; run: make demo-up"
MODE=${MODE:-normal} PROFILE=${PROFILE:-0}
RATE=${RATE:-1000} HOLD=${HOLD:-30s} RAMP=${RAMP:-10s} IDLE=${IDLE:-10s} IDLE_RATE=${IDLE_RATE:-20}
DUP_RATE=${DUP_RATE:-0.05}
case $MODE in
  normal) DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-600} ;;
  control) DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-1200} ;;
  *) die "MODE must be normal or control, got $MODE" ;;
esac
prefix="CAd$(openssl rand -hex 3)"
idle_s=$(secs "$IDLE") ramp_s=$(secs "$RAMP") hold_s=$(secs "$HOLD")
(( PROFILE == 0 || hold_s >= 40 )) || die "PROFILE=1 needs HOLD of at least 40s"
work=$(mktemp -d)

# value KEY TEXT : KEY's value from KEY=value lines in TEXT.
value() { sed -n "s/^ *$1=//p" <<<"$2" | tail -1; }

read -r _ _ _ _ dlq_before <<<"$(remote "$sut" /opt/cep/stats.sh)"
alerts_before=$(remote "$sut" "$ALERT_COUNT") || die "can't read the alert inbox"

# Control removes the limit for this run only: a runtime drop-in, gone on reboot and removed
# at exit, so inference never stays overloaded by accident.
control_conf=/run/systemd/system/cep@inference.service.d/control.conf
cleanup() {
  [[ -n ${sampler:-} ]] && kill "$sampler" 2>/dev/null
  if [[ $MODE == control ]]; then
    remote "$sut" "rm -f $control_conf && systemctl daemon-reload && systemctl restart cep@inference" >/dev/null ||
      todo "could not restore inference's normal workers; run: make demo-up"
  fi
  rm -rf "$work"
}
trap cleanup EXIT
if [[ $MODE == control ]]; then
  act "control: restarting inference with 200 workers (the model's capacity is 4)"
  remote "$sut" "mkdir -p ${control_conf%/*} && printf '[Service]\nEnvironment=INFERENCE_WORKERS=200\n' >$control_conf &&
    systemctl daemon-reload && systemctl restart cep@inference && sleep 2 && systemctl is-active cep@inference" >/dev/null ||
    die "could not restart inference in control mode"
fi
since=$(remote "$sut" "date +%s" | tr -d ' ')

if (( PROFILE )); then
  # Runs on the server by itself, so the profile starts on time whatever SSM's delays: 10 s into
  # the hold, 30 s each, both services at once.
  at=$(( idle_s + ramp_s + 10 + 8 )) # + about 8 s for k6 to start through SSM
  remote "$sut" "systemd-run --quiet --collect --unit=cep-prof-$prefix sh -c '
    sleep $at
    curl -sf -o /var/tmp/$prefix-ingest.pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30 &
    curl -sf -o /var/tmp/$prefix-attribution.pprof http://127.0.0.1:6061/debug/pprof/profile?seconds=30
    wait'" >/dev/null || die "could not schedule the profiles"
fi

act "spike ($MODE): idle ${IDLE_RATE}/s for $IDLE, ramp to ${RATE}/s over $RAMP, hold $HOLD (run $prefix)"
# The backlog, every few seconds, while k6 runs: it rises during the spike and drains after.
(
  while sleep 4; do
    read -r ap _ ip _ _ <<<"$(remote "$sut" /opt/cep/stats.sh 2>/dev/null)" || continue
    printf '    backlog: attribution %6s   inference %6s\n' "$ap" "$ip"
    echo "$ap $ip" >>"$work/lag"
  done
) &
sampler=$!
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

want_insights=$accepted
[[ $MODE == control ]] && want_insights=0
act "k6 done; waiting for both consumers to drain (up to ${DRAIN_TIMEOUT}s)"
res=$(remote "$sut" "/opt/cep/drain.sh $prefix $accepted $DRAIN_TIMEOUT $dlq_before $want_insights \
  $idle_s $(( idle_s + ramp_s )) $(( idle_s + ramp_s + hold_s )) $since") || die "drain check failed"
kill $sampler 2>/dev/null || true
rows=$(value ROWS "$res") insights=$(value INSIGHTS "$res") drained=$(value DRAINED_S "$res")
dead=$(value NEW_DEAD_LETTERS "$res")
peak_a=$(cut -d' ' -f1 "$work/lag" 2>/dev/null | sort -n | tail -1) peak_i=$(cut -d' ' -f2 "$work/lag" 2>/dev/null | sort -n | tail -1)
alerts=$(( $(remote "$sut" "$ALERT_COUNT") - alerts_before ))

if (( PROFILE )); then
  mkdir -p "$demo/.build/profiles"
  for svc in ingest attribution; do
    fetch_file "$sut" "/var/tmp/$prefix-$svc.pprof" "$demo/.build/profiles/$prefix-$svc.pprof" ||
      todo "no $svc profile"
  done
fi

fails=0
check() { # check DESCRIPTION CONDITION...
  local what=$1; shift
  if "$@"; then ok "$what"; else todo "$what"; fails=$((fails + 1)); fi
}
# Model p95 in the spike within 10% of idle (normal), or at least twice it, or timeouts
# (control: the overload the limit prevents). Fractional, so awk.
flat() { awk -v s="$(value SPIKE_M95 "$res")" -v i="$(value IDLE_M95 "$res")" 'BEGIN { exit !(i > 0 && s <= i * 1.1) }'; }
overloaded() {
  awk -v s="$(value SPIKE_M95 "$res")" -v i="$(value IDLE_M95 "$res")" -v t="$(value MODEL_TIMEOUTS "$res")" \
    'BEGIN { exit !(t > 0 || (i > 0 && s >= 2 * i)) }'
}
echo
echo "Run $prefix ($MODE): $(value REQUESTS "$k6out") requests at up to ${RATE}/s, $(value DUPLICATES_SENT "$k6out") deliberate duplicates"
echo "  edge latency (k6): p50 $(value P50_MS "$k6out") ms, p95 $(value P95_MS "$k6out") ms, p99 $(value P99_MS "$k6out") ms"
echo "  model latency:     idle p95 $(value IDLE_M95 "$res") ms ($(value IDLE_N "$res")), spike p95 $(value SPIKE_M95 "$res") ms ($(value SPIKE_N "$res")); $(value MODEL_TIMEOUTS "$res") timeouts"
echo "  time to insight:   idle p95 $(value IDLE_T95 "$res") s, spike p95 $(value SPIKE_T95 "$res") s"
echo "  peak backlog:      attribution ${peak_a:-?}, inference ${peak_i:-?} (sampled every few seconds)"
echo "  drained ${drained} s after the spike; $insights insights; $dead dead letters; $alerts alert emails"
check "no failed requests ($(value FAILED_RATE "$k6out") failed, $(value UNEXPECTED "$k6out") unexpected)" \
  test "$(value FAILED_RATE "$k6out")" = 0 -a "$(value UNEXPECTED "$k6out")" = 0
check "nothing lost or duplicated: $rows calls stored for $accepted accepted ($sent sent)" \
  test "$rows" -ge "$accepted" -a "$rows" -le "$sent"
check "drained within ${DRAIN_TIMEOUT}s" test "$drained" != none
check "no alert emails during the run" test "$alerts" = 0
if [[ $MODE == normal ]]; then
  check "every call classified: $insights insights" test "$insights" -ge "$accepted" -a "$insights" -le "$sent"
  check "no dead letters" test "$dead" = 0
  check "model latency flat: spike p95 within 10% of idle" flat
else
  check "model overloaded: spike p95 at least twice idle, or timeouts" overloaded
  check "every call classified or dead-lettered: $insights + $dead" test $(( insights + dead )) -ge "$accepted"
fi
commit=$(git -C "$root" rev-parse --short HEAD)
echo
echo "Rows for docs/benchmarks.md:"
echo "| $(date +%F) | \`$commit\` | AWS c7g.xlarge (k6 on c7g.large) | ${RATE}/s, ramp $RAMP, hold $HOLD; + inference ($MODE) |" \
  "$(value P50_MS "$k6out") / $(value P99_MS "$k6out") ms | $(value FAILED_RATE "$k6out") | ~${peak_a:-?} | ${drained}s | $( (( rows >= accepted && rows <= sent )) && echo ✓ || echo ✗) $rows | $dead |"
echo "| $(date +%F) | \`$commit\` | $MODE, $([[ $MODE == control ]] && echo 200 || echo 4) workers, AWS | $(value IDLE_M95 "$res") / $(value SPIKE_M95 "$res") ms |" \
  "$(value IDLE_T95 "$res") s / $(value SPIKE_T95 "$res") s | ~${peak_i:-?} | ${drained}s | $insights | $(value MODEL_TIMEOUTS "$res") | $dead |"
# An explicit exit: otherwise the EXIT trap's last command would set the status.
exit $(( fails > 0 ))
