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
