#!/usr/bin/env bash
# Tests that the load tests never put the database password on a command line, where any
# local user can read it (ps, /proc/<pid>/cmdline): pg_use's parsing, and a check of the
# scripts themselves. Offline; no database needed.
set -uo pipefail
cd "$(dirname "$0")" || exit 1
fails=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad() { printf '  \033[31m✗\033[0m %s\n' "$*"; fails=$((fails + 1)); }

# shellcheck source=pg.sh
source ./pg.sh

# pg_use splits the URL into libpq's variables, decoding %-escapes in the password.
(
  pg_use 'postgres://leads_app:s3cr%40t%2Fx@127.0.0.1:5433/leads' || exit 1
  [ "$PGUSER|$PGPASSWORD|$PGHOST|$PGPORT|$PGDATABASE" = 'leads_app|s3cr@t/x|127.0.0.1|5433|leads' ] || exit 1
  env | grep -q '^PGPASSWORD=' # exported, so psql and child processes see it
) && ok "pg_use exports the URL's parts as PG* variables" || bad "pg_use: wrong PG* variables"
(
  pg_use 'postgresql://u:p@db.internal/x?sslmode=disable' || exit 1
  [ "$PGPORT|$PGDATABASE" = '5432|x' ]
) && ok "pg_use defaults the port and drops query options" || bad "pg_use: port default or query"
(
  PGHOST=stale; pg_use 'postgresql://postgres@/postgres' || exit 1 # the AWS demo: socket, peer auth
  [ "$PGUSER|${PGPASSWORD-unset}|${PGHOST-unset}|$PGDATABASE" = 'postgres||unset|postgres' ]
) && ok "pg_use takes a socket URL without a password (the AWS demo's)" || bad "pg_use: socket URL"
if (pg_use 'not a url' 2>/dev/null); then bad "pg_use accepted a non-URL"; else ok "pg_use refuses a non-URL"; fi
msg=$(pg_use 'mysql://u:hunter2@h/db' 2>&1 >/dev/null)
[[ $msg != *hunter2* ]] && ok "pg_use's error doesn't print the URL" || bad "pg_use printed the password"

# No script hands a database URL to a command as an argument.
for f in run.sh crash.sh; do
  hits=$(grep -nE '(psql|env)\b[^#]*\$\{?(DATABASE_URL|DB_URL)\b|\$\{?(DATABASE_URL|DB_URL)\}?["'"'"']? *(-c|-q|-A)' "$f" || true)
  [ -z "$hits" ] && ok "$f passes no database URL as an argument" || bad "$f passes a database URL as an argument:
$hits"
done

[ "$fails" = 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }
