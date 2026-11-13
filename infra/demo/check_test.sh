#!/usr/bin/env bash
# Tests check.sh offline: a fake `aws` on PATH answers as AWS would with resources left over,
# with nothing left and when not signed in, so nothing reaches AWS.
set -uo pipefail
cd "$(dirname "$0")" || exit 1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad() { printf '  \033[31m✗\033[0m %s\n' "$*"; fails=$((fails + 1)); }

mkdir -p "$tmp/bin" "$tmp/repo/infra/demo" "$tmp/repo/infra/bootstrap/aws"
cat >"$tmp/bin/aws" <<'SHIM'
#!/usr/bin/env bash
# Fake AWS CLI. FAKE_STATE: leftovers | clean | nologin. LEFT: which kind is left over.
args="$*"
if [[ $args == *"sts get-caller-identity"* ]]; then [[ $FAKE_STATE == nologin ]] && exit 255; echo 1; exit 0; fi
left() { if [[ $FAKE_STATE == leftovers && ${LEFT:-all} =~ ^(all|$1)$ ]]; then echo "$2"; else echo "${3-}"; fi; }
case "$args" in
  *"ec2 describe-instances"*) left instance i-0abc ;;
  *"ec2 describe-volumes"*) left volume vol-0abc ;;
  *"ec2 describe-security-groups"*) left sg sg-0abc ;;
  *"s3api list-buckets"*) left bucket call-event-pipeline-demo-2026 None ;;
  *"iam list-roles"*) left role call-event-pipeline-demo ;;
  *"iam list-instance-profiles"*) left profile call-event-pipeline-demo ;;
  *"ssm get-parameters-by-path"*) left param /call-event-pipeline/demo/TWILIO_AUTH_TOKEN ;;
  *) echo "fake aws: unexpected call: $args" >&2; exit 2 ;;
esac
SHIM
chmod +x "$tmp/bin/aws"
cp check.sh lib.sh "$tmp/repo/infra/demo/"
echo 'PROJECT_PROFILE=proj-admin REGION=us-east-1' >"$tmp/repo/infra/bootstrap/aws/config.env"

run() { # run STATE [LEFT] -> sets out and code
  out=$(PATH="$tmp/bin:$PATH" FAKE_STATE=$1 LEFT=${2:-all} bash "$tmp/repo/infra/demo/check.sh" 2>&1)
  code=$?
}

echo "check.sh"
run clean
if (( code == 0 )) && [[ $out == *"Nothing left"* && $out != *"✗"* ]]; then ok "nothing left: exits 0"
else bad "nothing left: exit $code"$'\n'"$out"; fi
for kind in instance volume sg bucket role profile param; do
  run leftovers "$kind"
  if (( code != 0 )) && [[ $(grep -c "✗" <<<"$out") == 1 ]]; then ok "a leftover $kind: exits $code and lists it"
  else bad "a leftover $kind: exit $code"$'\n'"$out"; fi
done
run leftovers all
if (( code != 0 )) && [[ $(grep -c "✗" <<<"$out") == 7 ]]; then ok "everything left: exits $code, seven ✗"
else bad "everything left: exit $code"$'\n'"$out"; fi
run nologin
if (( code != 0 )) && [[ $out == *"aws sso login"* ]]; then ok "not signed in: exits $code and says how to sign in"
else bad "not signed in: exit $code"$'\n'"$out"; fi
(( fails == 0 ))
