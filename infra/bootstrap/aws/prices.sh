#!/usr/bin/env bash
# Read-only: looks up the demo environment's on-demand prices in AWS's Price List API (free)
# and prints what one session costs. Uses the management profile; prices are the same for
# every account. Run it before the first `make demo-up`, and whenever the instance types change.
source "$(dirname "$0")/lib.sh"
ensure_login "$MGMT_PROFILE"

LOCATION="US East (N. Virginia)" # us-east-1, as REGION
SESSION_HOURS=${SESSION_HOURS:-3}  # every instance terminates itself after 3 hours
DISK_GB=${DISK_GB:-20}             # root volume per instance

# price FILTER... : the on-demand USD price per unit of the one matching product.
price() {
  local filters=() f
  for f in "$@"; do filters+=("Type=TERM_MATCH,Field=${f%%=*},Value=${f#*=}"); done
  mgmt pricing get-products --region us-east-1 --service-code AmazonEC2 --filters "${filters[@]}" \
    --query 'PriceList' --output json | python3 -c '
import json, sys
products = [json.loads(p) for p in json.load(sys.stdin)]
if len(products) != 1:
    sys.exit(f"expected one product, found {len(products)}")
dims = [d for t in products[0]["terms"]["OnDemand"].values() for d in t["priceDimensions"].values()]
print(dims[0]["pricePerUnit"]["USD"])'
}
instance() {
  price instanceType="$1" location="$LOCATION" operatingSystem=Linux tenancy=Shared preInstalledSw=NA \
    capacitystatus=Used marketoption=OnDemand
}

act "asking the Price List API (on-demand, ${LOCATION})"
sut=$(instance c7g.xlarge)
load=$(instance c7g.large)
gp3=$(price productFamily=Storage volumeApiName=gp3 location="$LOCATION")
python3 - "$sut" "$load" "$gp3" "$SESSION_HOURS" "$DISK_GB" <<'PY'
import sys
sut, load, gp3 = (float(x) for x in sys.argv[1:4])
hours, disk = float(sys.argv[4]), float(sys.argv[5])
disk_hourly = 2 * disk * gp3 / 730  # GB-month prorated over a 730-hour month
session = (sut + load + disk_hourly) * hours
print(f"  c7g.xlarge (system under test)  ${sut:.4f}/hour")
print(f"  c7g.large  (load generator)     ${load:.4f}/hour")
print(f"  gp3 volume                      ${gp3:.3f}/GB-month ({2 * disk:.0f} GB: ${disk_hourly:.4f}/hour)")
print(f"  one {hours:.0f}-hour session          ${session:.2f}")
print(f"  sessions within the $10 budget  {int(10 // session)}")
PY
