#!/usr/bin/env bash
# Creates .env from .env.example, or adds any variables .env.example has gained since.
# Each "change-me" placeholder becomes a random secret. Existing values are never changed.
set -euo pipefail
cd "$(dirname "$0")/.."

touch .env
chmod 600 .env
while IFS= read -r line; do
  [[ "$line" =~ ^([A-Z_]+)= ]] || continue # skip comments and blank lines
  key=${BASH_REMATCH[1]}
  grep -q "^${key}=" .env && continue
  echo "${line//change-me/$(openssl rand -hex 24)}" >> .env
  echo "Added ${key} to .env"
done < .env.example
