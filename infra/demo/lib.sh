# shellcheck shell=bash
# Shared helpers for the demo environment's scripts. Sourced, not run.
# Settings come from the AWS bootstrap's config.env (PROJECT_PROFILE, REGION).
set -euo pipefail

demo="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$demo/../.." && pwd)"
config="$root/infra/bootstrap/aws/config.env"
[[ -f $config ]] || { echo "Missing $config; see infra/bootstrap/aws/README.md." >&2; exit 1; }
# shellcheck source=../bootstrap/aws/config.env.example
source "$config"
export AWS_PROFILE="$PROJECT_PROFILE" AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION"
export AWS_PAGER=""

PARAM_PATH=/call-event-pipeline/demo # the demo's generated secrets, deleted by demo-down

ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
todo() { printf '  \033[33m✗\033[0m %s\n' "$*"; }
act()  { printf '\033[36m→\033[0m %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

ensure_login() {
  aws sts get-caller-identity >/dev/null 2>&1 \
    || die "no valid session for profile $AWS_PROFILE. Run:  aws sso login --profile $AWS_PROFILE"
}

# out NAME : an OpenTofu output, or nothing if the environment doesn't exist. (Not
# `output -raw`: with no state it prints its warning on stdout, which reads as a value.)
out() {
  tofu -chdir="$demo" output -json 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], {}).get("value", ""))' "$1" \
    2>/dev/null || true
}

# wait_online INSTANCE_ID : until the instance's SSM agent has registered (about a minute).
wait_online() {
  local i status
  for ((i = 0; i < 60; i++)); do
    status=$(aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$1" \
      --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || true)
    [[ $status == Online ]] && return 0
    sleep 5
  done
  die "$1 did not register with Systems Manager within 5 minutes"
}

# remote INSTANCE_ID SCRIPT : runs SCRIPT (POSIX sh) as root through SSM Run Command, prints
# its output and returns non-zero unless it succeeded. No inbound port is involved: the
# instance's agent polls Systems Manager for commands.
remote() {
  local id=$1 script=$2 cmd status=Pending deadline=$((SECONDS + 960))
  # Explicit returns: callers use `remote … || die`, and bash ignores set -e in such calls.
  # The script isn't secret, so it can go on the command line (the CLI can't read a pipe).
  cmd=$(aws ssm send-command --instance-ids "$id" --document-name AWS-RunShellScript \
    --timeout-seconds 900 --query Command.CommandId --output text \
    --parameters "$(python3 -c 'import json,sys; print(json.dumps({"commands": [sys.argv[1]]}))' "$script")") \
    || return 1
  while [[ $status =~ ^(Pending|InProgress|Delayed)$ ]]; do
    (( SECONDS < deadline )) || { echo "    no result from $id after 16 minutes" >&2; return 1; }
    sleep 3
    status=$(aws ssm get-command-invocation --command-id "$cmd" --instance-id "$id" \
      --query Status --output text 2>/dev/null || echo Pending)
  done
  aws ssm get-command-invocation --command-id "$cmd" --instance-id "$id" \
    --query '[StandardOutputContent, StandardErrorContent]' --output text | sed '/^None$/d; s/^/    /'
  [[ $status == Success ]]
}

# secs DURATION : a k6-style duration (90s, 2m, 1m30s, 0s) in whole seconds.
secs() {
  local d=$1 total=0 n unit
  while [[ $d =~ ^([0-9]+)([hms])(.*)$ ]]; do
    n=${BASH_REMATCH[1]} unit=${BASH_REMATCH[2]} d=${BASH_REMATCH[3]}
    case $unit in h) total=$((total + n * 3600)) ;; m) total=$((total + n * 60)) ;; s) total=$((total + n)) ;; esac
  done
  [[ -z $d ]] || die "can't read the duration $1"
  echo "$total"
}

# A script for `remote`: how many alert emails the mail sink on the system under test holds.
ALERT_COUNT="curl -sf http://127.0.0.1:8025/messages | python3 -c 'import json,sys; print(len(json.load(sys.stdin) or []))'"

# fetch_file INSTANCE_ID REMOTE_PATH LOCAL_PATH : copies a file back through Run Command, in
# base64 chunks under its 24,000-character output limit, and checks its SHA-256. The instances
# can only read from S3, and this keeps it that way.
fetch_file() {
  local id=$1 path=$2 dest=$3 len want off chunk=20000
  read -r len want <<<"$(remote "$id" "test -s $path && echo \$(base64 -w0 $path | wc -c) \$(sha256sum <$path | cut -d' ' -f1)" | tr -s ' \t\n' ' ')" ||
    return 1
  [[ -n $want ]] || return 1
  : >"$dest.b64"
  for ((off = 1; off <= len; off += chunk)); do
    remote "$id" "base64 -w0 $path | cut -c $off-$((off + chunk - 1))" | tr -d ' \t\n' >>"$dest.b64" || return 1
  done
  base64 -d "$dest.b64" >"$dest" && rm -f "$dest.b64"
  [[ $(sha256sum <"$dest" | cut -d' ' -f1) == "$want" ]] || { echo "    $dest: checksum mismatch" >&2; return 1; }
}
