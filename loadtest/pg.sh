# Sourced by the load tests. psql takes its connection from libpq's PG* variables, so the
# password in DATABASE_URL never goes on a command line, where any local user could read it
# (ps, /proc/<pid>/cmdline).

# pg_use <url>: export PGUSER, PGPASSWORD, PGHOST, PGPORT and PGDATABASE from a
# postgres://user[:password]@[host][:port]/database URL. An empty host means the local
# socket, so PGHOST is unset. %-escapes in the user and password are decoded; query options
# are dropped. The error never prints the URL.
pg_use() {
  local re='^postgres(ql)?://([^:@/]+)(:([^@]*))?@([^:/?]*)(:([0-9]+))?/([^?]+)'
  if ! [[ $1 =~ $re ]]; then
    echo "the database URL must look like postgres://user:password@host:port/database" >&2
    return 1
  fi
  local user=${BASH_REMATCH[2]} pass=${BASH_REMATCH[4]} host=${BASH_REMATCH[5]}
  export PGUSER PGPASSWORD PGPORT=${BASH_REMATCH[7]:-5432} PGDATABASE=${BASH_REMATCH[8]}
  printf -v PGUSER '%b' "${user//%/\\x}"
  printf -v PGPASSWORD '%b' "${pass//%/\\x}"
  if [ -n "$host" ]; then export PGHOST=$host; else unset PGHOST; fi
}

# The helpers below hide Postgres's NOTICEs for their own session only, such as
# 'database "x" does not exist, skipping' from DROP ... IF EXISTS on a fresh run. A NOTICE is
# routine but reads like a failure in the output; warnings and errors still print.
_pg_quiet() { psql -X -q -v ON_ERROR_STOP=1 -c "SET client_min_messages = warning" "$@"; }

# pg_recreate_db <name>: drop the database if it exists and create it empty.
pg_recreate_db() {
  _pg_quiet -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" -c "CREATE DATABASE $1"
}

# pg_drop_db <name>: drop the database if it exists.
pg_drop_db() {
  _pg_quiet -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)"
}
