#!/bin/sh
# drain.sh PREFIX ACCEPTED TIMEOUT_S DLQ_BEFORE: waits until both consumers are empty and every
# accepted webhook of this run (CallSids starting with PREFIX) is stored, then prints the run's
# results as shell assignments. Run as root on the system under test, through SSM.
prefix=$1 accepted=$2 timeout=$3 dlq_before=$4
q() { runuser -u postgres -- psql -X -tAq -d leads -c "$1"; }
start=$(date +%s) drained=""
while [ $(( $(date +%s) - start )) -le "$timeout" ]; do
  set -- $(/opt/cep/stats.sh)
  rows=$(q "SELECT count(*) FROM interactions WHERE provider_id LIKE '$prefix%'")
  insights=$(q "SELECT count(*) FROM insights WHERE provider_id LIKE '$prefix%'")
  if [ "$1" = 0 ] && [ "$2" = 0 ] && [ "$3" = 0 ] && [ "$4" = 0 ] && [ "$rows" -ge "$accepted" ] && [ "$insights" -ge "$accepted" ]; then
    drained=$(( $(date +%s) - start )); break
  fi
  sleep 2
done
set -- $(/opt/cep/stats.sh)
echo "DRAINED_S=${drained:-none}"
echo "ROWS=$(q "SELECT count(*) FROM interactions WHERE provider_id LIKE '$prefix%'")"
echo "INSIGHTS=$(q "SELECT count(*) FROM insights WHERE provider_id LIKE '$prefix%'")"
echo "NEW_DEAD_LETTERS=$(( $5 - dlq_before ))"
q "SELECT 'MODEL_P50=' || round(percentile_cont(0.5) WITHIN GROUP (ORDER BY model_ms)::numeric, 1),
          'MODEL_P95=' || round(percentile_cont(0.95) WITHIN GROUP (ORDER BY model_ms)::numeric, 1)
   FROM insights WHERE provider_id LIKE '$prefix%'" | tr '|' '\n'
