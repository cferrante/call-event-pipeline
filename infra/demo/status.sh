#!/usr/bin/env bash
# make demo-status: forwards the monitor's status page and the alert inbox from the system
# under test to this machine, through Session Manager (no inbound port). Ctrl-C to stop.
#   STATUS_PORT=18090 INBOX_PORT=18025 (local ports; defaults avoid the local services' ports)
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id)
[[ -n $sut ]] || die "no demo environment; run: make demo-up"
command -v session-manager-plugin >/dev/null \
  || die "needs the Session Manager plugin; see infra/demo/README.md"
STATUS_PORT=${STATUS_PORT:-18090} INBOX_PORT=${INBOX_PORT:-18025}

forward() { # forward REMOTE_PORT LOCAL_PORT
  aws ssm start-session --target "$sut" --document-name AWS-StartPortForwardingSession \
    --parameters "{\"portNumber\":[\"$1\"],\"localPortNumber\":[\"$2\"]}" >/dev/null &
}
forward 8090 "$STATUS_PORT"
forward 8025 "$INBOX_PORT"
trap 'kill $(jobs -p) 2>/dev/null' EXIT
for _ in $(seq 30); do curl -sf -o /dev/null "http://127.0.0.1:$STATUS_PORT/" && break; sleep 1; done
echo "Status page:  http://localhost:$STATUS_PORT"
echo "Alert inbox:  http://localhost:$INBOX_PORT/messages"
echo "Forwarding through Session Manager; Ctrl-C to stop."
wait
