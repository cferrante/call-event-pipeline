// Edge step-up test (docs/design.md, section 9): signed voice webhooks at a fixed arrival rate
// that steps up every STEP (default 500, 1,000, 2,000 … 8,000/s for 60 s each). Each step is
// its own scenario, so k6 reports its p99 and errors separately; the highest step with
// p99 < 50 ms and no errors is the edge's sustained rate. `make demo-stepup` runs it from the
// load generator and samples both machines' CPU, so a k6-bound result can't pass as the server's.
import http from 'k6/http';
import crypto from 'k6/crypto';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

const TARGET = __ENV.TARGET;
const PUBLIC_URL = __ENV.PUBLIC_URL;
const TOKEN = __ENV.TOKEN;
const STEPS = (__ENV.STEPS || '500,1000,2000,3000,4000,5000,6000,8000').split(',').map(Number);
const STEP = __ENV.STEP || '60s';
const PREFIX = __ENV.PREFIX || 'CAstep';
const SUMMARY_FILE = __ENV.SUMMARY_FILE || 'stepup.env';

const stepSeconds = Number(STEP.replace(/s$/, ''));
if (!(stepSeconds > 0)) throw new Error(`STEP must be in seconds, like 60s; got ${STEP}`);
const name = (rate) => `r${rate}`;

const scenarios = {};
const thresholds = {};
STEPS.forEach((rate, i) => {
  scenarios[name(rate)] = {
    executor: 'constant-arrival-rate',
    rate,
    timeUnit: '1s',
    duration: STEP,
    startTime: `${i * stepSeconds}s`,
    // Enough VUs that k6 never drops an iteration while the server is still fast: at 50 ms a
    // request, rate × 0.05 are in flight.
    preAllocatedVUs: Math.max(50, Math.ceil(rate * 0.05)),
    maxVUs: Math.max(500, rate),
    env: { RATE: String(rate) },
  };
  // A threshold per step makes k6 keep that step's own latency and error figures.
  thresholds[`http_req_duration{scenario:${name(rate)}}`] = ['p(99)<50'];
  thresholds[`http_req_failed{scenario:${name(rate)}}`] = ['rate==0'];
  thresholds[`dropped_iterations{scenario:${name(rate)}}`] = ['count==0'];
});

export const options = { scenarios, thresholds, summaryTrendStats: ['p(50)', 'p(99)', 'max'] };

const sent = new Counter('sent');
const TRACKING_NUMBERS = ['+18005550001', '+18005550002', '+18005550003', '+18005559999'];
const WEBHOOK_PATH = '/webhooks/voice';

function sign(params) {
  const data = Object.keys(params).sort().reduce((acc, k) => acc + k + params[k], PUBLIC_URL + WEBHOOK_PATH);
  return crypto.hmac('sha1', TOKEN, data, 'base64');
}

export default function () {
  const i = exec.scenario.iterationInTest;
  const params = {
    // Unique across steps: each step's iterations start again at 0.
    CallSid: PREFIX + __ENV.RATE.padStart(5, '0') + String(i).padStart(8, '0'),
    From: '+1415' + String(i % 5000).padStart(7, '0'),
    To: TRACKING_NUMBERS[i % TRACKING_NUMBERS.length],
  };
  const body = Object.keys(params).map((k) => `${k}=${encodeURIComponent(params[k])}`).join('&');
  sent.add(1);
  const res = http.post(TARGET + WEBHOOK_PATH, body, {
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Twilio-Signature': sign(params) },
  });
  if (res.status !== 200) {
    console.error(`unexpected response: step=${__ENV.RATE} status=${res.status} error="${res.error}"`);
  }
}

// One line per step: STEP_<rate>="requests p50 p99 failed_rate dropped".
export function handleSummary(data) {
  const m = data.metrics;
  const get = (metric, rate) => m[`${metric}{scenario:${name(rate)}}`];
  const lines = STEPS.map((rate) => {
    const d = get('http_req_duration', rate);
    const f = get('http_req_failed', rate);
    const dropped = get('dropped_iterations', rate);
    const reqs = f ? f.values.passes + f.values.fails : 0;
    return `STEP_${rate}="${reqs} ${d ? d.values['p(50)'].toFixed(2) : 0} ${d ? d.values['p(99)'].toFixed(2) : 0}` +
      ` ${f ? f.values.rate : 0} ${dropped ? dropped.values.count : 0}"`;
  });
  return { stdout: lines.join('\n') + '\n', [SUMMARY_FILE]: lines.join('\n') + '\n' };
}
