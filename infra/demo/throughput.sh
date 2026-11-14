#!/usr/bin/env bash
# make demo-throughput: attribution's sustained throughput. With the consumers and monitor
# stopped, the load generator queues about RATE × QUEUE webhooks (default 1,000/s for 100 s);
# then attribution starts alone and drains them, timed, with its CPU, Postgres's CPU and a
# CPU profile, so the bottleneck can be named. Afterwards inference and the monitor restart
# and the run is checked like a spike: every webhook stored and classified once.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip)
[[ -n $sut && -n $loadgen ]] || die "no demo environment; run: make demo-up"
RATE=${RATE:-1000} QUEUE=${QUEUE:-100s}
prefix="CAt$(openssl rand -hex 3)"
value() { sed -n "s/^ *$1=//p" <<<"$2" | tail -1; }

read -r _ _ _ _ dlq_before <<<"$(remote "$sut" /opt/cep/stats.sh)"
act "stopping attribution, inference and the monitor"
remote "$sut" "systemctl stop cep@attribution cep@inference cep@monitor" >/dev/null || die "could not stop them"
trap 'remote "$sut" "systemctl start cep@attribution cep@inference cep@monitor" >/dev/null' EXIT

act "queueing ${RATE}/s for $QUEUE (run $prefix)"
k6out=$(remote "$loadgen" "set -a; . /etc/cep/config.env; . /run/cep/env; set +a
export TARGET=http://$sut_ip:8081 PUBLIC_URL=\"\$INGEST_PUBLIC_URL\" TOKEN=\"\$TWILIO_AUTH_TOKEN\"
export RATE=$RATE HOLD=$QUEUE RAMP=1s IDLE=0s DUP_RATE=0 PREFIX=$prefix SUMMARY_FILE=/tmp/summary.env
cd /tmp && /opt/cep/bin/k6 run --quiet /opt/cep/spike.js 2>/tmp/k6.err >/dev/null || true
cat /tmp/summary.env") || die "k6 failed on the load generator"
accepted=$(value UNIQUE_ACCEPTED "$k6out")
[[ -n $accepted ]] || die "k6 printed no summary"

act "starting attribution alone on $accepted queued webhooks"
res=$(remote "$sut" "/opt/cep/throughput.sh $prefix $accepted") || die "throughput run failed:"$'\n'"$res"
sed -n '/TIMELINE_START/,/DRAIN_S/p' <<<"$res" | sed '1d;$d'
mkdir -p "$demo/.build/profiles"
fetch_file "$sut" "/var/tmp/$prefix-attribution.pprof" "$demo/.build/profiles/$prefix-attribution-drain.pprof" ||
  todo "no attribution profile"

act "restarting inference and the monitor; waiting for every insight"
remote "$sut" "systemctl start cep@inference cep@monitor" >/dev/null
check=$(remote "$sut" "/opt/cep/drain.sh $prefix $accepted 900 $dlq_before") || die "drain check failed"
trap - EXIT

echo
echo "Run $prefix: $(value MESSAGES "$res") messages drained by attribution in $(value DRAIN_S "$res") s"
echo "  throughput:  $(value RATE "$res") messages/s"
echo "  CPU (% of one core): attribution $(value ATTRIBUTION_CPU "$res"), Postgres $(value POSTGRES_CPU "$res");" \
  "machine $(value MACHINE_CPU "$res")% of $(value CPUS "$res") cores, iowait $(value IOWAIT "$res")%"
echo "  profile:     infra/demo/.build/profiles/$prefix-attribution-drain.pprof"
fails=0
rows=$(value ROWS "$check") insights=$(value INSIGHTS "$check")
if [[ $rows == "$accepted" ]]; then ok "every webhook stored once: $rows"; else todo "rows $rows for $accepted accepted"; fails=1; fi
if [[ $insights == "$accepted" ]]; then ok "every webhook classified: $insights"; else todo "insights $insights for $accepted"; fails=1; fi
exit $fails
