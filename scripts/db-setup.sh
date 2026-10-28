#!/usr/bin/env bash
# One-time local setup: creates the leads_app role and the leads database, owned by it.
# Run as a Postgres superuser (your own login role via peer auth). Safe to re-run: it
# creates what's missing and resets leads_app's password to the one in .env.
set -euo pipefail
cd "$(dirname "$0")/.."

./scripts/env-setup.sh
set -a; source .env; set +a

# Extract the password from DATABASE_URL (postgres://leads_app:<password>@...).
password=$(sed -E 's#^postgres://[^:]+:([^@]+)@.*#\1#' <<<"$DATABASE_URL")

# The password goes in on stdin, never on the command line, so it can't show up in `ps`.
psql -X -q -v ON_ERROR_STOP=1 -d postgres <<SQL
\set pw '${password}'
SELECT 'CREATE ROLE leads_app'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'leads_app') \gexec
-- CREATEDB lets integration tests create throwaway databases; dev machines only.
ALTER ROLE leads_app WITH LOGIN NOSUPERUSER CREATEDB PASSWORD :'pw';
SELECT 'CREATE DATABASE leads OWNER leads_app'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'leads') \gexec
SQL
echo "leads_app role and leads database are ready."
