#!/usr/bin/env bash
# make demo-stepup: the edge step-up test (docs/design.md, section 9). Signed webhooks from the
# load generator at a rate that steps up every STEP, with both machines' CPU sampled, to find
# the highest rate the edge sustains with p99 < 50 ms and no errors.
#   STEPS=500,1000,2000,3000,4000,5000,6000,8000 STEP=60s make demo-stepup   (defaults)
#   PREALLOC=1000   virtual users started up front (default 5% of each step's rate)
# It measures the edge (ingest and NATS): the consumers and monitor are stopped during the
# test, and afterwards the stream is emptied and every service restarted, because the steps
# send far more webhooks than the consumers could drain in a session. Postgres is untouched.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip)
[[ -n $sut && -n $loadgen ]] || die "no demo environment; run: make demo-up"
STEPS=${STEPS:-500,1000,2000,3000,4000,5000,6000,8000} STEP=${STEP:-60s}
step_s=$(secs "$STEP") n=$(tr ',' '\n' <<<"$STEPS" | wc -l)
prefix="CAu$(openssl rand -hex 3)"
total=$(( n * step_s + 60 ))
(( total < 840 )) || die "$n steps of $STEP won't fit in one Run Command (15 minutes); use fewer or shorter steps"

# Every 2 s: epoch and the CPU counters (busy, total) from /proc/stat, for $total seconds.
sampler="systemd-run --quiet --collect --unit=cep-cpu-$prefix sh -c '
  end=\$((\$(date +%s) + $total))
  while [ \$(date +%s) -lt \$end ]; do
    read -r _ u n s i w q sq st _ </proc/stat
    echo \$(date +%s) \$((u + n + s + q + sq + st)) \$((u + n + s + i + w + q + sq + st))
    sleep 2
  done >/var/tmp/cpu-$prefix.log'"

act "stopping the consumers and monitor: this measures the edge alone"
remote "$sut" "systemctl stop cep@attribution cep@inference cep@monitor" >/dev/null || die "could not stop the consumers"
restore() {
  act "emptying the stream and restarting every service"
  remote "$sut" "/opt/cep/reset-stream.sh" >/dev/null || todo "the reset failed; run: make demo-up"
}
trap restore EXIT
remote "$sut" "$sampler" >/dev/null && remote "$loadgen" "$sampler" >/dev/null || die "could not start the CPU samplers"

act "stepping up: $STEPS per second, $STEP each (about $(( n * step_s / 60 )) minutes; run $prefix)"
k6out=$(remote "$loadgen" "set -a; . /etc/cep/config.env; . /run/cep/env; set +a
export TARGET=http://$sut_ip:8081 PUBLIC_URL=\"\$INGEST_PUBLIC_URL\" TOKEN=\"\$TWILIO_AUTH_TOKEN\"
export STEPS=$STEPS STEP=$STEP PREALLOC=${PREALLOC:-} PREFIX=$prefix SUMMARY_FILE=/tmp/stepup.env
echo START=\$(date +%s)
cd /tmp && /opt/cep/bin/k6 run --quiet /opt/cep/stepup.js 2>/tmp/k6-stepup.err >/dev/null || true
cat /tmp/stepup.env") || die "k6 failed on the load generator"
start=$(sed -n 's/^ *START=//p' <<<"$k6out")
sut_cpu=$(remote "$sut" "cat /var/tmp/cpu-$prefix.log") lg_cpu=$(remote "$loadgen" "cat /var/tmp/cpu-$prefix.log")

# cpu SAMPLES FROM TO : mean CPU busy % between two epochs, from the counters' differences.
cpu() {
  awk -v a="$2" -v b="$3" '$1 >= a && $1 <= b { if (!f) { b0 = $2; t0 = $3; f = 1 } b1 = $2; t1 = $3 }
    END { if (t1 > t0) printf "%.0f", 100 * (b1 - b0) / (t1 - t0); else printf "?" }' <<<"$1"
}
echo
printf '  %-7s %9s %9s %9s %8s %8s %12s %9s\n' "rate/s" requests "p50 ms" "p99 ms" failed dropped "load gen CPU" "SUT CPU"
best="" limited=""
i=0
for rate in ${STEPS//,/ }; do
  read -r reqs p50 p99 failed dropped <<<"$(sed -n "s/^ *STEP_$rate=\"\(.*\)\"/\1/p" <<<"$k6out")"
  from=$(( start + i * step_s + 5 )) to=$(( start + (i + 1) * step_s - 2 )) # skip each step's start
  lg=$(cpu "$lg_cpu" $from $to) su=$(cpu "$sut_cpu" $from $to)
  printf '  %-7s %9s %9s %9s %8s %8s %11s%% %8s%%\n' "$rate" "$reqs" "$p50" "$p99" "$failed" "$dropped" "$lg" "$su"
  if awk -v p="$p99" -v f="$failed" -v d="$dropped" 'BEGIN { exit !(p < 50 && f == 0 && d == 0) }'; then
    [[ $lg != "?" ]] && (( lg >= 80 )) && limited=${limited:-$rate}
    [[ -z $limited ]] && best=$rate
  fi
  i=$((i + 1))
done
echo
if [[ -n $best ]]; then ok "highest step with p99 < 50 ms, no errors and the load generator under 80% CPU: ${best}/s"; fi
[[ -n $limited ]] && todo "the load generator reached 80% CPU at ${limited}/s: higher steps measure k6, not the server"
echo "  STEPUP_BEST=${best:-none} LIMITED_AT=${limited:-none} COMMIT=$(git -C "$root" rev-parse --short HEAD)"
