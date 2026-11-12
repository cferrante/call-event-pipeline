#!/usr/bin/env bash
# Creates .env from .env.example, or adds any variables .env.example has gained since.
# Each "change-me" placeholder becomes a random secret, and the SendGrid simulator's key pair
# is generated. Existing values are never changed, and no value is ever printed.
set -euo pipefail
cd "$(dirname "$0")/.."

touch .env
chmod 600 .env

# value_of KEY prints KEY's value from .env (for deriving one value from another).
value_of() { grep -m1 "^$1=" .env | cut -d= -f2-; }

while IFS= read -r line; do
  [[ "$line" =~ ^([A-Z_]+)=(.*)$ ]] || continue # skip comments and blank lines
  key=${BASH_REMATCH[1]}
  value=${BASH_REMATCH[2]}
  grep -q "^${key}=" .env && continue
  case "$value" in
    generate-p256-private-key)
      # base64 PKCS#8 DER, the form Go's x509.ParsePKCS8PrivateKey reads.
      value=$(openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 |
        openssl pkcs8 -topk8 -nocrypt -outform DER | base64 -w0)
      ;;
    derive-from-simulator-private-key)
      # base64 DER SubjectPublicKeyInfo, the form SendGrid publishes its key in.
      value=$(value_of SENDGRID_SIMULATOR_PRIVATE_KEY | base64 -d |
        openssl pkey -inform DER -pubout -outform DER | base64 -w0)
      ;;
    *) value=${value//change-me/$(openssl rand -hex 24)} ;;
  esac
  printf '%s=%s\n' "$key" "$value" >> .env
  echo "Added ${key} to .env"
done < .env.example
