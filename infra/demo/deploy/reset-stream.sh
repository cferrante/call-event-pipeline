#!/bin/sh
# reset-stream.sh : empties JetStream (both streams and their consumers) and restarts every
# service, which recreate them. For the edge step-up, whose millions of webhooks are only there
# to load the edge and would otherwise take over an hour to drain. Postgres is untouched.
set -eu
services="cep@ingest.service cep@attribution.service cep@inference.service cep@monitor.service"
systemctl stop $services nats.service
rm -rf /var/lib/cep/nats/jetstream
systemctl start nats.service
for i in $(seq 30); do curl -sf http://127.0.0.1:8222/healthz >/dev/null && break; sleep 1; done
systemctl start $services
sleep 3
systemctl is-active nats.service $services
