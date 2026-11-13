#!/usr/bin/env bash
# make demo-check: read-only. Lists anything the demo environment created that still exists,
# and exits non-zero if there is any. Run after demo-down: it should say "Nothing left".
source "$(dirname "$0")/lib.sh"
ensure_login
TAG=Name=tag:project,Values=call-event-pipeline
left=0

# report WHAT IDS : ✓ when IDS is empty, else ✗ with the IDs.
report() {
  local what=$1 ids=${2:-}
  [[ $ids == None ]] && ids=""
  if [[ -z ${ids//[[:space:]]/} ]]; then ok "no $what"
  else todo "$what: $(tr -s '[:space:]' ' ' <<<"$ids")"; left=$((left + 1)); fi
}

echo "Demo environment leftovers ($AWS_PROFILE, $AWS_REGION)"
report "instances" "$(aws ec2 describe-instances --filters "$TAG" \
  Name=instance-state-name,Values=pending,running,shutting-down,stopping,stopped \
  --query 'Reservations[].Instances[].InstanceId' --output text)"
report "volumes" "$(aws ec2 describe-volumes --filters "$TAG" --query 'Volumes[].VolumeId' --output text)"
report "security groups" "$(aws ec2 describe-security-groups --filters "$TAG" \
  --query 'SecurityGroups[].GroupId' --output text)"
report "artifact buckets" "$(aws s3api list-buckets \
  --query "Buckets[?starts_with(Name, 'call-event-pipeline-demo-')].Name" --output text)"
report "IAM roles" "$(aws iam list-roles \
  --query "Roles[?starts_with(RoleName, 'call-event-pipeline-demo')].RoleName" --output text)"
report "instance profiles" "$(aws iam list-instance-profiles \
  --query "InstanceProfiles[?starts_with(InstanceProfileName, 'call-event-pipeline-demo')].InstanceProfileName" \
  --output text)"
report "secrets under $PARAM_PATH" "$(aws ssm get-parameters-by-path --path "$PARAM_PATH" \
  --query 'Parameters[].Name' --output text)"

if (( left > 0 )); then
  echo "$left kind(s) of resource left. Run: make demo-down"
  exit 1
fi
echo "Nothing left."
