#!/usr/bin/env bash
# make demo-fault: the silent-failure drill and its recovery, on the server (docs/design.md,
# sections 8.3 and 5.3). Under steady load (RATE, default 50/s for HOLD 150s), attribution is
# swapped for its faultinject build and the fault turned on: events are acknowledged but not
# written, while the process stays healthy. The monitor must email [FIRING] acked_not_written
# within 60 s. Then the fault is turned off, the gap is replayed with -replay-from, and every
# accepted webhook must be stored exactly once; a second replay must change nothing.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip)
[[ -n $sut && -n $loadgen ]] || die "no demo environment; run: make demo-up"
RATE=${RATE:-50} HOLD=${HOLD:-150s} FAULT_AFTER=${FAULT_AFTER:-30} DETECT_LIMIT=${DETECT_LIMIT:-120}
prefix="CAf$(openssl rand -hex 3)"
value() { sed -n "s/^ *$1=//p" <<<"$2" | tail -1; }
# alert_after RULE STATE EPOCH : seconds from EPOCH to the first STATE email for RULE received
# after it, or nothing yet.
alert_after() {
  remote "$sut" "curl -sf http://127.0.0.1:8025/messages | python3 -c '
import json, sys, datetime
for m in json.load(sys.stdin) or []:
    t = datetime.datetime.fromisoformat(m[\"Received\"].replace(\"Z\", \"+00:00\")).timestamp()
    if \"[$2]\" in m[\"Subject\"] and \"$1\" in m[\"Subject\"] and t >= $3:
        print(\"%.1f\" % (t - $3)); break'" | tr -d ' '
}
fault_conf=/run/systemd/system/cep@attribution.service.d/fault.conf
restore() {
  remote "$sut" "rm -f $fault_conf && systemctl daemon-reload && systemctl restart cep@attribution" >/dev/null ||
    todo "could not restore attribution; run: make demo-up"
}
trap restore EXIT

act "swapping in attribution's faultinject build (fault off)"
remote "$sut" "mkdir -p ${fault_conf%/*} && printf '[Service]\nExecStart=\nExecStart=/opt/cep/bin/attribution-fault\n' >$fault_conf &&
  systemctl daemon-reload && systemctl restart cep@attribution && sleep 2 && systemctl is-active cep@attribution" >/dev/null ||
  die "could not start the drill build"

act "steady load: ${RATE}/s for $HOLD (run $prefix)"
remote "$loadgen" "systemd-run --quiet --collect --unit=cep-k6-$prefix sh -c '
  set -a; . /etc/cep/config.env; . /run/cep/env; set +a
  export TARGET=http://$sut_ip:8081 PUBLIC_URL=\"\$INGEST_PUBLIC_URL\" TOKEN=\"\$TWILIO_AUTH_TOKEN\"
  export RATE=$RATE HOLD=$HOLD RAMP=1s IDLE=0s DUP_RATE=0 PREFIX=$prefix SUMMARY_FILE=/tmp/$prefix.env
  cd /tmp && /opt/cep/bin/k6 run --quiet /opt/cep/spike.js >/dev/null 2>/tmp/$prefix.err'" >/dev/null || die "could not start k6"
sleep "$FAULT_AFTER"

# Replay from a few seconds before the fault: replay writes only what is missing, so starting
# early is harmless, and late would miss events.
read -r t_on replay_from <<<"$(remote "$sut" "date -u -d @\$((\$(date +%s) - 5)) +%Y-%m-%dT%H:%M:%SZ >/var/tmp/$prefix.from
  systemctl kill -s USR1 --kill-who=main cep@attribution && echo \$(date +%s.%N) \$(cat /var/tmp/$prefix.from)")"
[[ -n $replay_from ]] || die "could not turn the fault on"
ok "fault on at $(date -d "@${t_on%.*}" +%T): events acknowledged but not written"

detect=""
# Measured on the server's clock: the email's arrival in the sink against the fault's start.
for ((i = 0; i < DETECT_LIMIT / 3 && ${#detect} == 0; i++)); do
  sleep 3
  detect=$(alert_after acked_not_written FIRING "$t_on")
done
remote "$sut" "systemctl kill -s USR1 --kill-who=main cep@attribution" >/dev/null || die "could not turn the fault off"
t_off=$(date +%s)
[[ -n $detect ]] && ok "[FIRING] acked_not_written emailed ${detect} s after the fault" ||
  todo "no [FIRING] acked_not_written within ${DETECT_LIMIT} s"
act "fault off; waiting for the load to finish"
while remote "$loadgen" "systemctl is-active --quiet cep-k6-$prefix" >/dev/null 2>&1; do sleep 5; done
k6out=$(remote "$loadgen" "cat /tmp/$prefix.env; echo UNEXPECTED=\$(grep -c 'unexpected response' /tmp/$prefix.err)")
accepted=$(value UNIQUE_ACCEPTED "$k6out")
[[ -n $accepted ]] || die "k6 printed no summary"

count="runuser -u postgres -- psql -X -tAq -d leads -c \"SELECT count(*) FROM interactions WHERE provider_id LIKE '$prefix%'\""
remote "$sut" "for i in \$(seq 60); do set -- \$(/opt/cep/stats.sh); [ \$1\$2 = 00 ] && break; sleep 2; done" >/dev/null
lost=$(( accepted - $(remote "$sut" "$count" | tr -d ' ') ))

act "replaying from $replay_from"
replay="set -a; . /etc/cep/config.env; . /run/cep/env; set +a; s=\$(date +%s.%N)
  /opt/cep/bin/attribution -replay-from $replay_from 2>&1 | grep 'replay finished'; echo TOOK=\$(awk -v a=\$s -v b=\$(date +%s.%N) 'BEGIN { printf \"%.1f\", b - a }')"
first=$(remote "$sut" "$replay") || die "replay failed:"$'\n'"$first"
after=$(remote "$sut" "$count" | tr -d ' ')
second=$(remote "$sut" "$replay") || die "second replay failed"
after2=$(remote "$sut" "$count" | tr -d ' ')

restore; trap - EXIT
act "normal attribution restored; waiting for [RESOLVED]"
resolved=""
for ((i = 0; i < 40 && ${#resolved} == 0; i++)); do sleep 3; resolved=$(alert_after acked_not_written RESOLVED "$t_off"); done

recorded() { sed -n 's/.* recorded=\([0-9]*\).*/\1/p' <<<"$1"; }
echo
echo "Drill $prefix: $accepted webhooks accepted at ${RATE}/s; fault on for $(( t_off - ${t_on%.*} )) s"
echo "  detection:     ${detect:-none} s to [FIRING] acked_not_written (window: the monitor's default)"
echo "  silently lost: $lost interactions before the replay"
echo "  replay:        $(recorded "$first") recorded in $(value TOOK "$first") s; rows $after for $accepted accepted"
echo "  second replay: $(recorded "$second") recorded; rows $after2"
echo "  resolved:      ${resolved:-no} s after the fault was turned off"
fails=0
check() { local what=$1; shift; if "$@"; then ok "$what"; else todo "$what"; fails=$((fails + 1)); fi; }
check "detected within 60 s" awk -v d="${detect:-999}" 'BEGIN { exit !(d <= 60) }'
check "the fault lost data (the drill really broke something): $lost" test "$lost" -gt 0
check "replay restored every row: $after = $accepted" test "$after" = "$accepted"
check "a second replay changed nothing" test "$(recorded "$second")" = 0 -a "$after2" = "$accepted"
check "the alert resolved" test -n "$resolved"
exit $(( fails > 0 ))
