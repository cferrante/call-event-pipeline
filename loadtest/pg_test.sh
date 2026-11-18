#!/usr/bin/env bash
# Tests that the load tests never put the database password on a command line, where any
# local user can read it (ps, /proc/<pid>/cmdline): pg_use's parsing, and a check of the
# scripts themselves. Offline; no database needed.
# With DATABASE_URL set, it also checks pg_recreate_db and pg_drop_db against that server,
# using a throwaway database named leads_pgtest. REQUIRE_DB=1 (set in CI) makes a missing
# DATABASE_URL a failure instead of a skip.
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

# Both scripts recreate and drop their database through the helpers, never a raw DROP, so a
# routine NOTICE can't reach the output.
for f in run.sh crash.sh; do
  hits=$(grep -n 'DROP DATABASE' "$f" || true)
  [ -z "$hits" ] && ok "$f drops databases only through pg.sh" || bad "$f runs DROP DATABASE itself:
$hits"
done

if [ -z "${DATABASE_URL:-}" ]; then
  if [ "${REQUIRE_DB:-}" = 1 ]; then bad "REQUIRE_DB=1 but DATABASE_URL is unset"
  else echo "  - skipped the database checks: DATABASE_URL is unset"; fi
else
  (
    pg_use "$DATABASE_URL" || exit 1
    db=leads_pgtest pass=0
    exists() { [ "$(psql -X -A -t -c "SELECT count(*) FROM pg_database WHERE datname = '$db'")" = 1 ]; }
    check() { if eval "$2"; then ok "$1"; else bad "$1"; pass=1; fi; }
    psql -X -q -c "DROP DATABASE IF EXISTS $db" 2>/dev/null

    err=$(pg_recreate_db "$db" 2>&1 >/dev/null); rc=$?
    check "pg_recreate_db creates a missing database without a NOTICE" '[ $rc = 0 ] && [ -z "$err" ] && exists'
    PGDATABASE=$db psql -X -q -c "CREATE TABLE leftover (x int)"
    err=$(pg_recreate_db "$db" 2>&1 >/dev/null); rc=$?
    left=$(PGDATABASE=$db psql -X -A -t -c "SELECT count(*) FROM pg_tables WHERE tablename = 'leftover'")
    check "pg_recreate_db replaces an existing database with an empty one" '[ $rc = 0 ] && [ -z "$err" ] && [ "$left" = 0 ]'
    err=$(pg_drop_db "$db" 2>&1 >/dev/null); rc=$?
    check "pg_drop_db drops the database" '[ $rc = 0 ] && [ -z "$err" ] && ! exists'
    err=$(pg_drop_db "$db" 2>&1 >/dev/null); rc=$?
    check "pg_drop_db on a missing database prints nothing" '[ $rc = 0 ] && [ -z "$err" ]'
    err=$(pg_recreate_db "no such-name" 2>&1 >/dev/null); rc=$?
    check "pg_recreate_db still reports real errors" '[ $rc != 0 ] && [ -n "$err" ]'
    exit $pass
  ) || fails=$((fails + 1))
fi

[ "$fails" = 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }
