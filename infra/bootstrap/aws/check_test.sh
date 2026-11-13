#!/usr/bin/env bash
# Tests check.sh offline: a fake `aws` on PATH answers as AWS would in each state, so nothing
# reaches AWS. check.sh must exit non-zero while any step is not done, so it can serve as the
# bootstrap's test (✗ before a step, ✓ after).
set -uo pipefail
cd "$(dirname "$0")" || exit 1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad() { printf '  \033[31m✗\033[0m %s\n' "$*"; fails=$((fails + 1)); }

mkdir "$tmp/bin" "$tmp/conf"
cat >"$tmp/bin/aws" <<'SHIM'
#!/usr/bin/env bash
# Fake AWS CLI. FAKE_STATE: nologin | fresh | done | old-budget.
args="$*"
profile=$(sed -n 's/.*--profile \([^ ]*\).*/\1/p' <<<"$args")
if [[ $args == *"sts get-caller-identity"* ]]; then
  [[ $FAKE_STATE == nologin ]] && exit 255
  [[ $profile == proj-admin && $FAKE_STATE == fresh ]] && exit 255
  echo 111122223333; exit 0
fi
case "$args" in
  *"organizations describe-organization"*) echo 111122223333 ;;
  *"organizations list-accounts"*) [[ $FAKE_STATE == fresh ]] && echo None || echo 444455556666 ;;
  *"sso-admin list-instances"*) printf 'arn:aws:sso:::instance/ssoins-1\td-1\n' ;;
  *"sso-admin list-permission-sets"*) echo arn:aws:sso:::permissionSet/ps-1 ;;
  *"sso-admin describe-permission-set"*) echo AdministratorAccess ;;
  *"identitystore get-user-id"*) echo user-1 ;;
  *"sso-admin list-account-assignments"*) [[ $FAKE_STATE == fresh ]] && echo 0 || echo 1 ;;
  *"budgets describe-notifications-for-budget"*)
    if [[ $FAKE_STATE == old-budget ]]; then printf 'ACTUAL\t1.0\nACTUAL\t100.0\nFORECASTED\t100.0\n'
    else printf 'ACTUAL\t50.0\nACTUAL\t100.0\nFORECASTED\t100.0\n'; fi ;;
  *"ssm describe-parameters"*) echo SecureString ;;
  *) echo "fake aws: unexpected call: $args" >&2; exit 2 ;;
esac
SHIM
chmod +x "$tmp/bin/aws"

# check.sh reads config.env next to it; run a copy of the scripts with a test config.
cp check.sh lib.sh secret.sh "$tmp/conf/"
cat >"$tmp/conf/config.env" <<'EOF'
MGMT_PROFILE=mgmt-admin SSO_SESSION=s PROJECT_ACCOUNT_NAME=call-event-pipeline PROJECT_ACCOUNT_EMAIL=x@example.com
SSO_USERNAME=me PERMISSION_SET_NAME=AdministratorAccess PROJECT_PROFILE=proj-admin REGION=us-east-1
BUDGET_NAME=monthly-10-usd BUDGET_LIMIT_USD=10 BUDGET_EMAIL=x@example.com GROQ_KEY_PARAM=/call-event-pipeline/groq-api-key
EOF

run() { # run STATE -> sets out and code
  out=$(PATH="$tmp/bin:$PATH" FAKE_STATE=$1 bash "$tmp/conf/check.sh" 2>&1)
  code=$?
}

echo "check.sh"
run fresh
if (( code != 0 )) && [[ $out == *"✗"*"01 account"* ]]; then ok "before the scripts run: exits $code, account ✗"
else bad "before the scripts run: exit $code (want non-zero with the account ✗)"$'\n'"$out"; fi
run nologin
if (( code != 0 )) && [[ $out == *"aws sso login"* ]]; then ok "not signed in: exits $code and says how to sign in"
else bad "not signed in: exit $code"$'\n'"$out"; fi
run old-budget
if (( code != 0 )) && [[ $out == *"✗"*"03 budget"* ]]; then ok "budget alerts at the wrong thresholds: exits $code, budget ✗"
else bad "wrong budget thresholds: exit $code"$'\n'"$out"; fi
run done
if (( code == 0 )) && [[ $out != *"✗"* ]]; then ok "everything done: exits 0, all ✓"
else bad "everything done: exit $code"$'\n'"$out"; fi
(( fails == 0 ))
