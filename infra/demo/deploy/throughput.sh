#!/bin/sh
# throughput.sh PREFIX EXPECTED : starts attribution (stopped, with EXPECTED messages of run
# PREFIX queued), and measures how fast it drains them: messages/s, and the CPU used by
# attribution, Postgres and the whole machine meanwhile, so the bottleneck can be named. A 30 s
# CPU profile of attribution is saved to /var/tmp/PREFIX-attribution.pprof. Run as root on the
# system under test, through SSM. Inference stays stopped, so attribution has Postgres to itself.
set -eu
prefix=$1 expected=$2
hz=$(getconf CLK_TCK) cpus=$(nproc)
pending() { set -- $(/opt/cep/stats.sh); echo $(( $1 + $2 )); }
# ticks PID... : CPU ticks (user + system) used so far by these processes.
ticks() { t=0; for p in "$@"; do [ -r /proc/$p/stat ] && t=$(( t + $(awk '{print $14 + $15}' /proc/$p/stat) )); done; echo $t; }
# sys : the machine's busy, total and iowait ticks so far.
sys() { awk '/^cpu /{print $2+$3+$4+$7+$8+$9, $2+$3+$4+$5+$6+$7+$8+$9, $6}' /proc/stat; }
calc() { awk "BEGIN { printf \"%.${2:-0}f\", $1 }"; }

before=$(pending)
[ "$before" -ge "$expected" ] || { echo "only $before messages queued, expected $expected" >&2; exit 1; }
pg_pids=$(pgrep -u postgres -d ' ' postgres)
pg0=$(ticks $pg_pids); set -- $(sys); busy0=$1 all0=$2 io0=$3
t0=$(date +%s.%N)
systemctl start cep@attribution
pid=$(systemctl show -p MainPID --value cep@attribution)
(sleep 10; curl -sf -o "/var/tmp/$prefix-attribution.pprof" 'http://127.0.0.1:6061/debug/pprof/profile?seconds=30') &
echo "TIMELINE_START"
last=$before
while :; do
  sleep 5
  now=$(pending)
  echo "  $(calc "$(date +%s.%N) - $t0") s  pending $now  ($(( (last - now) / 5 ))/s)"
  last=$now
  [ "$now" -eq 0 ] && break
done
t1=$(date +%s.%N)
a1=$(ticks $pid); pg1=$(ticks $(pgrep -u postgres -d ' ' postgres)); set -- $(sys)
wait
el=$(calc "$t1 - $t0" 3)
rows=$(runuser -u postgres -- psql -X -tAq -d leads -c "SELECT count(*) FROM interactions WHERE provider_id LIKE '$prefix%'")
echo "DRAIN_S=$(calc "$el" 1)"
echo "MESSAGES=$before"
echo "RATE=$(calc "$before / $el")"
echo "ROWS=$rows"
# As % of one core (attribution, Postgres) and of the whole machine ($cpus cores).
echo "ATTRIBUTION_CPU=$(calc "100 * $a1 / $hz / $el")"
echo "POSTGRES_CPU=$(calc "100 * ($pg1 - $pg0) / $hz / $el")"
echo "MACHINE_CPU=$(calc "100 * ($1 - $busy0) / ($2 - $all0)")"
echo "IOWAIT=$(calc "100 * ($3 - $io0) / ($2 - $all0)")"
echo "CPUS=$cpus"
