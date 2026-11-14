#!/usr/bin/env bash
# make demo-nats-sync SYNC=always|default : sets JetStream's disk sync on the system under test
# and prints what NATS reports. Run demo-spike afterwards to measure the edge with it.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id)
[[ -n $sut ]] || die "no demo environment; run: make demo-up"
[[ ${SYNC:-} == always || ${SYNC:-} == default ]] || die "set SYNC=always or SYNC=default"
remote "$sut" "/opt/cep/nats-sync.sh $SYNC" || die "could not set the sync interval"
