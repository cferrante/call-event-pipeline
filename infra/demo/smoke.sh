#!/usr/bin/env bash
# make demo-smoke: proves the environment works end to end, through Systems Manager only.
# Exits non-zero on the first failure (and before the environment exists).
source "$(dirname "$0")/lib.sh"
ensure_login
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip)
[[ -n $sut && -n $loadgen ]] || die "no demo environment; run: make demo-up"
id="SMsmoke$(openssl rand -hex 13)"

act "system under test: /healthz, services, expiry timer"
remote "$sut" 'set -e
curl -fsS http://127.0.0.1:8081/healthz; echo
units="nats cep@ingest cep@attribution cep@inference cep@monitor cep@mailsink"
systemctl is-active --quiet $units || { systemctl is-active $units; exit 1; }
echo "services active: $units"
systemctl is-active --quiet demo-expiry.timer
echo "expiry timer: powers off in $(( $(cat /var/lib/demo-expiry) - $(date +%s) )) s"' \
  || die "system under test failed its checks"

act "load generator: /healthz and a signed SMS webhook across the private network"
remote "$loadgen" "set -e
curl -fsS http://$sut_ip:8081/healthz; echo
set -a; . /etc/cep/config.env; . /run/cep/env; set +a
reply=\$(/opt/cep/bin/sendhook -channel sms -id $id -target http://$sut_ip:8081)
echo \"\$reply\" | head -1
echo \"\$reply\" | grep -q '200 OK'
systemctl is-active --quiet demo-expiry.timer
echo \"expiry timer: powers off in \$(( \$(cat /var/lib/demo-expiry) - \$(date +%s) )) s\"" \
  || die "load generator failed its checks"

act "the webhook reached Postgres"
remote "$sut" "for i in 1 2 3 4 5 6 7 8 9 10; do
  n=\$(runuser -u postgres -- psql -X -tAq -d leads -c \"SELECT count(*) FROM interactions WHERE provider_id = '$id'\")
  [ \"\$n\" = 1 ] && { echo 'interaction stored'; exit 0; }
  sleep 1
done
echo 'interaction not stored after 10 s' >&2; exit 1" || die "the webhook was not stored"
ok "smoke test passed"
