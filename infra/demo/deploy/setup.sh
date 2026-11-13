#!/bin/sh
# Installs the release unpacked in /opt/cep and starts this instance's role. Run as root by
# `make demo-up` through Systems Manager; safe to re-run, which is how a new release ships.
#   setup.sh SUT_PRIVATE_IP
set -eu
sut_ip=$1
. /etc/cep/instance.env # ROLE and AWS_DEFAULT_REGION, from cloud-init

id cep >/dev/null 2>&1 || useradd --system --home-dir /var/lib/cep --create-home --shell /usr/sbin/nologin cep
install -m 0644 /opt/cep/units/* /etc/systemd/system/

# Settings that aren't secret. Signatures cover the URL the sender called, so both
# instances use the system under test's private address.
cat > /etc/cep/config.env <<CONF
NATS_URL=nats://127.0.0.1:4222
INGEST_PORT=8081
INGEST_PUBLIC_URL=http://$sut_ip:8081
MONITOR_ADDR=127.0.0.1:8090
MAILSINK_ADDR=127.0.0.1:8025
SENDGRID_BASE_URL=http://127.0.0.1:8025
ALERT_EMAIL_FROM=monitor@example.com
ALERT_EMAIL_TO=oncall@example.com
CONF

systemctl daemon-reload
systemctl restart cep-secrets.service

if [ "$ROLE" = sut ]; then
  # The app role and database, as scripts/db-setup.sh does locally. The password goes in on
  # stdin, never on a command line.
  password=$(sed -n 's#^DATABASE_URL=postgres://[^:]*:\([^@]*\)@.*#\1#p' /run/cep/env)
  [ -n "$password" ] || { echo "DATABASE_URL is not among the secrets" >&2; exit 1; }
  runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -d postgres <<SQL
\set pw '$password'
SELECT 'CREATE ROLE leads_app' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'leads_app') \gexec
ALTER ROLE leads_app WITH LOGIN NOSUPERUSER NOCREATEDB PASSWORD :'pw';
SELECT 'CREATE DATABASE leads OWNER leads_app' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'leads') \gexec
SQL
  services="nats.service cep@ingest.service cep@attribution.service cep@inference.service cep@mailsink.service cep@monitor.service"
  systemctl enable --quiet $services
  systemctl restart $services
  sleep 3
  systemctl is-active $services
fi
echo "$ROLE ready"
