#!/usr/bin/env bash
# make demo-down: destroys everything demo-up created, deletes the demo's secrets, then
# checks that nothing is left.
source "$(dirname "$0")/lib.sh"
ensure_login

act "destroying (OpenTofu)"
tofu -chdir="$demo" init -input=false >/dev/null
tofu -chdir="$demo" destroy -input=false -auto-approve | grep -E '^Destroy complete' || true

act "deleting the demo's secrets"
names=$(aws ssm get-parameters-by-path --path "$PARAM_PATH" --query 'Parameters[].Name' --output text)
if [[ -n $names && $names != None ]]; then
  # shellcheck disable=SC2086 # one argument per name
  aws ssm delete-parameters --names $names --query 'length(DeletedParameters)' --output text
fi

"$demo/check.sh"
