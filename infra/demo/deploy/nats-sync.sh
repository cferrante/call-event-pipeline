#!/bin/sh
# nats-sync.sh default|always [--no-restart] : sets JetStream's disk sync and restarts NATS.
# default syncs every 2 minutes (a power loss could lose up to that much acknowledged data);
# always syncs every write before acknowledging it (docs/design.md, section 5.5). Prints the
# setting NATS reports, so the benchmark records what was measured rather than what was asked.
set -eu
case ${1:-} in
  default) extra="" ;;
  always) extra="sync_interval: always" ;;
  *) echo "usage: nats-sync.sh default|always [--no-restart]" >&2; exit 2 ;;
esac
cat >/etc/cep/nats.conf <<CONF
jetstream {
  store_dir: "/var/lib/cep/nats"
  $extra
}
CONF
[ "${2:-}" = --no-restart ] && exit 0
systemctl restart nats.service
for i in $(seq 30); do curl -sf http://127.0.0.1:8222/healthz >/dev/null && break; sleep 1; done
# The consumers and ingest reconnect by themselves; give them a moment.
sleep 3
curl -sf http://127.0.0.1:8222/varz | python3 -c '
import json, sys
c = json.load(sys.stdin)["jetstream"]["config"]
print("SYNC=" + ("always" if c.get("sync_always") else "every %ds" % (c["sync_interval"] // 10**9)))'
