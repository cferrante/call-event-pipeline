#!/usr/bin/env bash
# make demo-crash: the crash test (loadtest/crash.sh) on the system under test, with the
# release's binaries and its own database and NATS server, so the demo's data is untouched.
#   KILLS=on RATE=100 DURATION=240s SEED=1 MIN_KILLS=20 make demo-crash    (defaults)
#   KILLS=0 make demo-crash                                                 the clean run
# It runs on the server rather than from the load generator because it kills and restarts the
# services itself; at 100/s, k6 sharing the machine doesn't matter.
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id)
[[ -n $sut ]] || die "no demo environment; run: make demo-up"
KILLS=${KILLS:-on} RATE=${RATE:-100} DURATION=${DURATION:-240s} SEED=${SEED:-1} MIN_KILLS=${MIN_KILLS:-20}
commit=$(git -C "$root" rev-parse --short HEAD)
act "crash test on the server: KILLS=$KILLS, ${RATE}/s for $DURATION, seed $SEED (about 6 minutes)"
# As the postgres user, which can create the test's own database through the local socket
# (peer authentication: no password anywhere).
remote "$sut" "mkdir -p /var/tmp/crash && chown postgres /var/tmp/crash && cd /opt/cep &&
  runuser -u postgres -- env PATH=/opt/cep/bin:/usr/bin:/bin HOME=/var/tmp/crash \
    DATABASE_URL=postgresql://postgres@/postgres BIN_DIR=/opt/cep/bin COMMIT=$commit \
    MACHINE='AWS c7g.xlarge' RESULTS_ROOT=/var/tmp/crash KILLS=$KILLS RATE=$RATE DURATION=$DURATION \
    SEED=$SEED MIN_KILLS=$MIN_KILLS ./loadtest/crash.sh >/var/tmp/crash/out.txt 2>&1
  s=\$?; tail -32 /var/tmp/crash/out.txt; exit \$s"
