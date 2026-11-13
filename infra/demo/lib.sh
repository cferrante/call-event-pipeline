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
