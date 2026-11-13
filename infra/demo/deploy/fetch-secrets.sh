#!/bin/sh
# Writes every parameter under /call-event-pipeline/demo as NAME=value to /run/cep/env:
# root-only, on tmpfs, so secrets never reach the disk. Run by cep-secrets.service.
set -eu
umask 077
mkdir -p /run/cep
tab=$(printf '\t')
/snap/bin/aws ssm get-parameters-by-path --path /call-event-pipeline/demo --with-decryption \
  --query 'Parameters[].[Name,Value]' --output text |
  while IFS="$tab" read -r name value; do printf '%s=%s\n' "${name##*/}" "$value"; done \
  > /run/cep/env.tmp
[ -s /run/cep/env.tmp ] || { echo "no parameters under /call-event-pipeline/demo" >&2; exit 1; }
mv /run/cep/env.tmp /run/cep/env
echo "fetched $(wc -l < /run/cep/env) secrets"
