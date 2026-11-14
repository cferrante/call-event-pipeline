#!/bin/sh
# drain.sh PREFIX ACCEPTED TIMEOUT_S DLQ_BEFORE [WANT_INSIGHTS IDLE_S SPIKE_FROM_S SPIKE_TO_S SINCE_EPOCH]
# Waits until both consumers are empty and every accepted webhook of this run (CallSids
# starting with PREFIX) is stored and has WANT_INSIGHTS insights (default ACCEPTED; control
# mode passes 0, because the overloaded model dead-letters some), then prints the run's results
# as shell assignments. Run as root on the system under test, through SSM.
#
# With the window arguments it also prints model latency and time to insight for the idle
# window [0, IDLE_S) and the spike window [SPIKE_FROM_S, SPIKE_TO_S), in seconds from the run's
# first webhook, as loadtest/run.sh does, and the model timeouts logged since SINCE_EPOCH.
prefix=$1 accepted=$2 timeout=$3 dlq_before=$4 want_insights=${5:-$2}
idle_s=${6:-} spike_from=${7:-} spike_to=${8:-} since=${9:-}
q() { ${PSQL:-runuser -u postgres -- psql -d leads} -X -tAq -c "$1"; }
stats() { ${STATS:-/opt/cep/stats.sh}; }
start=$(date +%s) drained=""
while [ $(( $(date +%s) - start )) -le "$timeout" ]; do
  set -- $(stats)
  rows=$(q "SELECT count(*) FROM interactions WHERE provider_id LIKE '$prefix%'")
  insights=$(q "SELECT count(*) FROM insights WHERE provider_id LIKE '$prefix%'")
  if [ "$1" = 0 ] && [ "$2" = 0 ] && [ "$3" = 0 ] && [ "$4" = 0 ] && [ "$rows" -ge "$accepted" ] && [ "$insights" -ge "$want_insights" ]; then
    drained=$(( $(date +%s) - start )); break
  fi
  sleep 2
done
set -- $(stats)
echo "DRAINED_S=${drained:-none}"
echo "ROWS=$(q "SELECT count(*) FROM interactions WHERE provider_id LIKE '$prefix%'")"
echo "INSIGHTS=$(q "SELECT count(*) FROM insights WHERE provider_id LIKE '$prefix%'")"
echo "NEW_DEAD_LETTERS=$(( $5 - dlq_before ))"
q "SELECT 'MODEL_P50=' || round(percentile_cont(0.5) WITHIN GROUP (ORDER BY model_ms)::numeric, 1),
          'MODEL_P95=' || round(percentile_cont(0.95) WITHIN GROUP (ORDER BY model_ms)::numeric, 1)
   FROM insights WHERE provider_id LIKE '$prefix%'" | tr '|' '\n'
[ -n "$spike_to" ] || exit 0

# window NAME FROM_S TO_S: NAME_N, NAME_M50, NAME_M95 (model ms), NAME_T95 (time to insight, s).
# Windows start at the run's first interaction, not insight: in control mode early messages may
# have no insight (dead-lettered), which would shift both windows.
window() {
  q "WITH t AS (SELECT min(occurred_at) AS t0 FROM interactions WHERE provider_id LIKE '$prefix%'),
          w AS (SELECT model_ms, extract(epoch FROM created_at - received_at) AS tti
                FROM insights, t
                WHERE provider_id LIKE '$prefix%'
                  AND received_at >= t0 + make_interval(secs => $2)
                  AND received_at <  t0 + make_interval(secs => $3))
     SELECT '$1_N=' || count(*),
            '$1_M50=' || coalesce(round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY model_ms)::numeric, 2), 0),
            '$1_M95=' || coalesce(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY model_ms)::numeric, 2), 0),
            '$1_T95=' || coalesce(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY tti)::numeric, 1), 0)
     FROM w" | tr '|' '\n'
}
window IDLE 0 "$idle_s"
window SPIKE "$spike_from" "$spike_to"
if [ -n "$since" ]; then
  echo "MODEL_TIMEOUTS=$(journalctl -u cep@inference --since "@$since" -o cat 2>/dev/null | grep -c 'model overloaded')"
fi
