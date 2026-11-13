#!/usr/bin/env bash
# Read-only: looks up the demo environment's on-demand prices in AWS's Price List API (free)
# and prints what one session costs. Uses the management profile; prices are the same for
# every account. Run it before the first `make demo-up`, and whenever the instance types change.
source "$(dirname "$0")/lib.sh"
ensure_login "$MGMT_PROFILE"

LOCATION="US East (N. Virginia)" # us-east-1, as REGION
SESSION_HOURS=${SESSION_HOURS:-3}  # every instance terminates itself after 3 hours
DISK_GB=${DISK_GB:-20}             # root volume per instance

# price SERVICE FILTER... : the on-demand USD price per unit of the one matching product.
price() {
  local service=$1 filters=() f
  shift
  for f in "$@"; do filters+=("Type=TERM_MATCH,Field=${f%%=*},Value=${f#*=}"); done
  mgmt pricing get-products --region us-east-1 --service-code "$service" --filters "${filters[@]}" \
    --query 'PriceList' --output json | python3 -c '
import json, sys
products = [json.loads(p) for p in json.load(sys.stdin)]
if len(products) != 1:
    sys.exit(f"expected one product, found {len(products)}")
dims = [d for t in products[0]["terms"]["OnDemand"].values() for d in t["priceDimensions"].values()]
print(dims[0]["pricePerUnit"]["USD"])'
}
instance() {
  price AmazonEC2 instanceType="$1" location="$LOCATION" operatingSystem=Linux tenancy=Shared preInstalledSw=NA \
    capacitystatus=Used marketoption=OnDemand
}

act "asking the Price List API (on-demand, ${LOCATION})"
sut=$(instance c7g.xlarge)
load=$(instance c7g.large)
gp3=$(price AmazonEC2 productFamily=Storage volumeApiName=gp3 location="$LOCATION")
# Each instance has a public IPv4 address, so it can reach Session Manager without paid VPC
# endpoints; AWS charges for every public IPv4 address.
ipv4=$(price AmazonVPC usagetype=USE1-PublicIPv4:InUseAddress)
python3 - "$sut" "$load" "$gp3" "$ipv4" "$SESSION_HOURS" "$DISK_GB" <<'PY'
import sys
sut, load, gp3, ipv4 = (float(x) for x in sys.argv[1:5])
hours, disk = float(sys.argv[5]), float(sys.argv[6])
disk_hourly = 2 * disk * gp3 / 730  # GB-month prorated over a 730-hour month
session = (sut + load + disk_hourly + 2 * ipv4) * hours
rows = [
    ("c7g.xlarge (system under test)", f"${sut:.4f}/hour"),
    ("c7g.large (load generator)", f"${load:.4f}/hour"),
    ("gp3 volumes", f"${gp3:.3f}/GB-month ({2 * disk:.0f} GB: ${disk_hourly:.4f}/hour)"),
    ("public IPv4 addresses", f"${ipv4:.4f}/hour each (2: ${2 * ipv4:.4f}/hour)"),
    (f"one {hours:.0f}-hour session", f"${session:.2f}"),
    ("sessions within the $10 budget", str(int(10 // session))),
]
for name, value in rows:
    print(f"  {name:<32}{value}")
PY
