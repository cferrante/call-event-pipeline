#!/usr/bin/env bash
# make demo-up: creates the demo environment, ships the current code to it and smoke-tests
# it. Safe to re-run; a re-run ships a new release to the same instances.
#   EXPIRY_MINUTES=N  power off (and terminate) N minutes after first boot; default and max 180
source "$(dirname "$0")/lib.sh"
ensure_login

NATS_VERSION=v2.15.1
NATS_SHA256=e2fbab4cad7d3c76e3052eb959570393c9d73ba756b70bf8e9eea61e9ae6d49c # linux-arm64
build="$demo/.build"
started=$EPOCHREALTIME

act "planning (OpenTofu)"
tofu -chdir="$demo" init -input=false >/dev/null
tofu -chdir="$demo" plan -input=false -out=demo.tfplan -var "expiry_minutes=${EXPIRY_MINUTES:-180}" >/dev/null
# The guard before any apply: no inbound access, and every instance terminates itself.
tofu -chdir="$demo" show -json demo.tfplan | (cd "$root" && go run ./infra/demo/plancheck)

act "applying"
tofu -chdir="$demo" apply -input=false demo.tfplan | grep -E '^(Apply complete|.*: Creation complete)' || true
rm -f "$demo/demo.tfplan"
sut=$(out sut_id) loadgen=$(out loadgen_id) sut_ip=$(out sut_private_ip) bucket=$(out bucket)
[[ -n $sut && -n $loadgen ]] || die "the apply produced no instances"

act "building the release for linux/arm64"
rm -rf "$build/release" && mkdir -p "$build/release/bin" "$build/cache"
(cd "$root" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$build/release/bin/" \
  ./cmd/ingest ./cmd/attribution ./cmd/inference ./cmd/monitor ./cmd/mailsink ./cmd/sendhook)
nats="nats-server-$NATS_VERSION-linux-arm64"
if [[ ! -f $build/cache/$nats.tar.gz ]]; then
  curl -fsSL -o "$build/cache/$nats.tar.gz" \
    "https://github.com/nats-io/nats-server/releases/download/$NATS_VERSION/$nats.tar.gz"
fi
echo "$NATS_SHA256  $build/cache/$nats.tar.gz" | sha256sum -c --quiet - || die "nats-server checksum mismatch"
tar -xzf "$build/cache/$nats.tar.gz" -C "$build/cache" "$nats/nats-server"
cp "$build/cache/$nats/nats-server" "$build/release/bin/"
cp -r "$demo/deploy/." "$build/release/"
tar -czf "$build/release.tar.gz" -C "$build/release" .
aws s3 cp --only-show-errors "$build/release.tar.gz" "s3://$bucket/release.tar.gz"
ok "release uploaded ($(du -h "$build/release.tar.gz" | cut -f1))"

act "storing the demo's secrets in SSM Parameter Store (generated once per environment)"
# put NAME : stores stdin as a SecureString. The value never goes on a command line, in a file
# on disk or in OpenTofu's state: the AWS CLI can't read JSON from a pipe, so it passes through
# a private file on an in-memory tmpfs that is removed straight after.
secrets_dir=$(mktemp -d "${XDG_RUNTIME_DIR:-/dev/shm}/demo-secrets.XXXXXX")
trap 'rm -rf "$secrets_dir"' EXIT
[[ $(stat -f -c %T "$secrets_dir") == tmpfs ]] || die "$secrets_dir is not in memory (tmpfs)"
put() {
  local f="$secrets_dir/param.json"
  (umask 077; python3 -c 'import json,sys; print(json.dumps({"Name": sys.argv[1], "Type": "SecureString",
    "Value": sys.stdin.read().strip(), "Overwrite": True}))' "$PARAM_PATH/$1" >"$f")
  aws ssm put-parameter --cli-input-json "file://$f" >/dev/null
  rm -f "$f"
}
have=$(aws ssm get-parameters-by-path --path "$PARAM_PATH" --query 'length(Parameters)' --output text)
if [[ $have != 5 ]]; then
  printf 'postgres://leads_app:%s@127.0.0.1:5432/leads' "$(openssl rand -hex 24)" | put DATABASE_URL
  openssl rand -hex 24 | put TWILIO_AUTH_TOKEN
  printf 'SG.%s' "$(openssl rand -hex 24)" | put SENDGRID_API_KEY
  # The SendGrid simulator's key pair, as scripts/env-setup.sh makes it.
  key=$(openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 |
    openssl pkcs8 -topk8 -nocrypt -outform DER | base64 -w0)
  printf '%s' "$key" | put SENDGRID_SIMULATOR_PRIVATE_KEY
  printf '%s' "$key" | base64 -d | openssl pkey -inform DER -pubout -outform DER | base64 -w0 | put SENDGRID_PUBLIC_KEY
  unset key
  ok "5 secrets generated"
else
  ok "secrets already present"
fi

install="set -e
. /etc/cep/instance.env; export AWS_DEFAULT_REGION
cloud-init status --wait >/dev/null || [ \$? -eq 2 ] # 2: finished with warnings
/snap/bin/aws s3 cp --only-show-errors s3://$bucket/release.tar.gz /tmp/release.tar.gz
rm -rf /opt/cep && mkdir -p /opt/cep && tar -xzf /tmp/release.tar.gz -C /opt/cep
/opt/cep/setup.sh $sut_ip"
for role in sut loadgen; do
  id=$sut; [[ $role == loadgen ]] && id=$loadgen
  act "installing on $role ($id): waits for first boot to finish"
  wait_online "$id"
  remote "$id" "$install" || die "install on $role failed"
done

"$demo/smoke.sh"
printf '\nDemo environment up in %.0f s. Instances terminate themselves %s minutes after first boot.\n' \
  "$(bc <<<"$EPOCHREALTIME - $started")" "${EXPIRY_MINUTES:-180}"
echo "Tear down with: make demo-down"
