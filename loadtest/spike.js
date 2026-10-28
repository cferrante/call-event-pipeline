// Spike test for the ingest edge: sends signed voice webhooks at a fixed arrival rate,
// re-sending some as duplicates the way Twilio retries. Run it via loadtest/run.sh, which
// starts isolated services and checks that every accepted webhook reached Postgres.
import http from 'k6/http';
import crypto from 'k6/crypto';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

const TARGET = __ENV.TARGET;         // where requests go, e.g. http://127.0.0.1:8091
const PUBLIC_URL = __ENV.PUBLIC_URL; // what the signature covers (the service's INGEST_PUBLIC_URL)
const TOKEN = __ENV.TOKEN;           // the service's TWILIO_AUTH_TOKEN
const RATE = Number(__ENV.RATE || 1000);
const RAMP = __ENV.RAMP || '10s';
const HOLD = __ENV.HOLD || '60s';
const IDLE = __ENV.IDLE || '0s'; // a gentle period before the spike, as a baseline
const IDLE_RATE = Number(__ENV.IDLE_RATE || 0);
const DUP_RATE = Number(__ENV.DUP_RATE || 0.05);
const SUMMARY_FILE = __ENV.SUMMARY_FILE || 'summary.env';

// Arrival rate, not a fixed number of virtual users: requests keep coming at RATE per second
// however slow the server gets, as real webhook traffic does (no coordinated omission).
export const options = {
  scenarios: {
    spike: {
      executor: 'ramping-arrival-rate',
      startRate: IDLE !== '0s' ? IDLE_RATE : 0,
      timeUnit: '1s',
      preAllocatedVUs: 200,
      maxVUs: 2000,
      stages: [
        ...(IDLE !== '0s' ? [{ target: IDLE_RATE, duration: IDLE }] : []),
        { target: RATE, duration: RAMP },
        { target: RATE, duration: HOLD },
        { target: 0, duration: '5s' },
      ],
    },
  },
  thresholds: {
    http_req_failed: ['rate==0'],
    http_req_duration: ['p(99)<50'],
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

const uniqueAccepted = new Counter('unique_accepted'); // distinct CallSids that got a 200
const uniqueSent = new Counter('unique_sent'); // distinct CallSids sent, whatever the response
const duplicatesSent = new Counter('duplicates_sent');

const TRACKING_NUMBERS = ['+18005550001', '+18005550002', '+18005550003', '+18005559999']; // last is unknown
const CALLERS = 5000;
const WEBHOOK_PATH = '/webhooks/voice';

// Twilio's scheme: URL + each parameter name and value, names sorted, HMAC-SHA1, base64.
function sign(params) {
  const data = Object.keys(params).sort().reduce((acc, k) => acc + k + params[k], PUBLIC_URL + WEBHOOK_PATH);
  return crypto.hmac('sha1', TOKEN, data, 'base64');
}

// post sends one webhook and reports whether it got a 200. Anything else is logged with its
// status and error, so a failed run says what failed (status 0 means no HTTP response at all).
function post(body, req, id) {
  const res = http.post(TARGET + WEBHOOK_PATH, body, req);
  if (res.status !== 200) {
    console.error(`unexpected response: id=${id} status=${res.status} error_code=${res.error_code} error="${res.error}"`);
  }
  return res.status === 200;
}

export default function () {
  const i = exec.scenario.iterationInTest;
  const params = {
    CallSid: 'CAk6' + String(i).padStart(10, '0'),
    From: '+1415' + String(i % CALLERS).padStart(7, '0'),
    To: TRACKING_NUMBERS[i % TRACKING_NUMBERS.length],
  };
  const body = Object.keys(params).map((k) => `${k}=${encodeURIComponent(params[k])}`).join('&');
  const req = {
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Twilio-Signature': sign(params) },
    tags: { name: 'voice webhook' },
  };

  uniqueSent.add(1);
  let accepted = post(body, req, params.CallSid);
  if (Math.random() < DUP_RATE) {
    duplicatesSent.add(1);
    accepted = post(body, req, params.CallSid) || accepted;
  }
  if (accepted) {
    uniqueAccepted.add(1);
  }
}

// Writes the numbers run.sh needs as shell variables, plus a short human-readable summary.
export function handleSummary(data) {
  const m = data.metrics;
  const d = m.http_req_duration.values;
  const v = {
    REQUESTS: m.http_reqs.values.count,
    FAILED_RATE: m.http_req_failed.values.rate,
    UNIQUE_ACCEPTED: m.unique_accepted ? m.unique_accepted.values.count : 0,
    UNIQUE_SENT: m.unique_sent ? m.unique_sent.values.count : 0,
    DUPLICATES_SENT: m.duplicates_sent ? m.duplicates_sent.values.count : 0,
    PEAK_RPS: RATE,
    P50_MS: d['p(50)'].toFixed(2),
    P95_MS: d['p(95)'].toFixed(2),
    P99_MS: d['p(99)'].toFixed(2),
    MAX_MS: d.max.toFixed(2),
  };
  const env = Object.entries(v).map(([k, val]) => `${k}=${val}`).join('\n') + '\n';
  const text =
    `k6: ${v.REQUESTS} requests, ${(v.FAILED_RATE * 100).toFixed(3)}% failed, ` +
    `${v.UNIQUE_ACCEPTED} unique accepted, ${v.DUPLICATES_SENT} duplicates sent\n` +
    `latency ms: p50 ${v.P50_MS}  p95 ${v.P95_MS}  p99 ${v.P99_MS}  max ${v.MAX_MS}\n`;
  return { stdout: text, [SUMMARY_FILE]: env };
}
