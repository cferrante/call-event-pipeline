#!/bin/sh
# Prints "attribution_pending attribution_ack_pending inference_pending inference_ack_pending
# dead_letters" from NATS's monitoring endpoint. Used by the demo scripts through SSM.
curl -sf "http://127.0.0.1:8222/jsz?streams=true&consumers=true" | python3 -c '
import json, sys
streams = {s["name"]: s for a in json.load(sys.stdin).get("account_details", []) for s in a.get("stream_detail", [])}
consumers = {c["name"]: c for c in streams.get("LEADS", {}).get("consumer_detail", [])}
out = []
for name in ("attribution", "inference"):
    c = consumers.get(name, {})
    out += [c.get("num_pending", 0), c.get("num_ack_pending", 0)]
out.append(streams.get("LEADS_DLQ", {}).get("state", {}).get("messages", 0))
print(*out)'
