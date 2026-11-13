#!/usr/bin/env bash
# Read-only: reports which bootstrap steps are done, and exits non-zero while any is not.
# Changes nothing. Run it before and after each step: ✗ turning into ✓ is the test.
source "$(dirname "$0")/lib.sh"
missing=0
need() { todo "$@"; missing=$((missing + 1)); }

echo "Management account ($MGMT_PROFILE)"
ensure_login "$MGMT_PROFILE"
if master=$(mgmt organizations describe-organization --query Organization.MasterAccountId --output text 2>/dev/null); then
  me=$(mgmt sts get-caller-identity --query Account --output text)
  if [[ $master == "$me" ]]; then ok "00 organization exists; this is its management account"
  else need "00 this account belongs to another organization; use its management account's profile"; fi
else
  need "00 no organization (set one up with cferrante-dev's 01-organization.sh)"
fi

account_id=$(project_account_id 2>/dev/null || echo None)
if [[ $account_id != None ]]; then ok "01 account $PROJECT_ACCOUNT_NAME exists"
else need "01 account $PROJECT_ACCOUNT_NAME"; fi

if [[ $account_id != None && -n ${SSO_USERNAME:-} ]]; then
  read -r instance_arn store_id <<<"$(sso_instance)"
  ps_arn=$(permission_set_arn "$instance_arn")
  user_id=$(sso_user_id "$store_id")
  n=$(mgmt sso-admin list-account-assignments --instance-arn "$instance_arn" --account-id "$account_id" \
    --permission-set-arn "$ps_arn" --query "length(AccountAssignments[?PrincipalId=='$user_id'])" --output text)
  if (( n > 0 )); then ok "02 $SSO_USERNAME has $PERMISSION_SET_NAME in the project account"
  else need "02 $SSO_USERNAME access to the project account"; fi
else
  need "02 sign-in access (needs step 01 and SSO_USERNAME)"
fi

echo "Project account ($PROJECT_PROFILE)"
if ! aws --profile "$PROJECT_PROFILE" sts get-caller-identity >/dev/null 2>&1; then
  need "02 profile $PROJECT_PROFILE missing or not signed in (aws sso login --profile $PROJECT_PROFILE)"
  need "03 budget (needs the profile)"
  need "04 Groq API key (needs the profile)"
  echo; echo "$missing step(s) to do"; exit 1
fi
ok "02 profile $PROJECT_PROFILE works"

# The alerts must be exactly the agreed ones: 50% and 100% actual, 100% forecast.
acct=$(project sts get-caller-identity --query Account --output text)
if alerts=$(project budgets describe-notifications-for-budget --account-id "$acct" --budget-name "$BUDGET_NAME" \
    --query 'Notifications[].[NotificationType,Threshold]' --output text 2>/dev/null); then
  have=$(awk '{ printf "%s %g\n", $1, $2 }' <<<"$alerts" | sort | paste -sd, -)
  if [[ $have == "ACTUAL 100,ACTUAL 50,FORECASTED 100" ]]; then ok "03 budget $BUDGET_NAME with alerts at 50%/100% actual, 100% forecast"
  else need "03 budget $BUDGET_NAME alerts are [$have]; re-run 03-budget.sh (and delete stray alerts in the console)"; fi
else
  need "03 budget $BUDGET_NAME"
fi

# Metadata only: the value is never read here.
if type=$(project ssm describe-parameters \
    --parameter-filters "Key=Name,Values=$GROQ_KEY_PARAM" \
    --query 'Parameters[0].Type' --output text 2>/dev/null) && [[ $type == SecureString ]]; then
  ok "04 Groq API key stored encrypted at $GROQ_KEY_PARAM"
else
  need "04 Groq API key at $GROQ_KEY_PARAM"
fi

echo
if (( missing > 0 )); then echo "$missing step(s) to do"; exit 1; fi
echo "Bootstrap complete"
