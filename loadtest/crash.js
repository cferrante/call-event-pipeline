// Crash test load: a fixed set of signed voice and SMS webhooks, sent at a steady rate and
// retried like a provider until each gets a 200, while loadtest/crash.sh kills services. Every
// field is a pure function of the iteration number, so crash.sh can compute in SQL exactly
// what the database must hold afterwards. Run it via crash.sh.
import http from 'k6/http';
import crypto from 'k6/crypto';
import exec from 'k6/execution';
import { sleep } from 'k6';
import { Counter } from 'k6/metrics';

const TARGET = __ENV.TARGET;
const PUBLIC_URL = __ENV.PUBLIC_URL;
const TOKEN = __ENV.TOKEN;
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || '240s';
const GIVE_UP_S = Number(__ENV.GIVE_UP_S || 300); // a provider stops retrying eventually
const SUMMARY_FILE = __ENV.SUMMARY_FILE || 'summary.env';

export const options = {
  scenarios: {
    crash: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      // While a service is down, every iteration in that window is retrying at once.
      preAllocatedVUs: 500,
      maxVUs: 5000,
    },
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(99)', 'max'],
};

const accepted = new Counter('accepted'); // webhooks that eventually got a 200
const gaveUp = new Counter('gave_up'); // webhooks never accepted within GIVE_UP_S
const retries = new Counter('retries'); // extra attempts after a failure

// Keep in step with expected() in crash.sh.
const TRACKING_NUMBERS = ['+18005550001', '+18005550002', '+18005550003', '+18005559999']; // last is unknown
const BODIES = ['what is the price?', 'I need help with my order', 'hello'];
const CALLERS = 500;

function sign(path, params) {
  const data = Object.keys(params).sort().reduce((acc, k) => acc + k + params[k], PUBLIC_URL + path);
  return crypto.hmac('sha1', TOKEN, data, 'base64');
}

// webhook returns iteration i's path and form parameters.
export function webhook(i) {
  const n = String(i).padStart(8, '0');
  const from = '+1415' + String(i % CALLERS).padStart(7, '0');
  const to = TRACKING_NUMBERS[i % TRACKING_NUMBERS.length];
  if (i % 2 === 0) {
    return ['/webhooks/voice', { CallSid: 'CAcrash' + n, From: from, To: to }];
  }
  return ['/webhooks/sms', { MessageSid: 'SMcrash' + n, From: from, To: to, Body: BODIES[i % BODIES.length] }];
}

export default function () {
  const i = exec.scenario.iterationInTest;
  const [path, params] = webhook(i);
  const body = Object.keys(params).map((k) => `${k}=${encodeURIComponent(params[k])}`).join('&');
  const req = {
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Twilio-Signature': sign(path, params) },
    tags: { name: path },
    timeout: '10s',
  };
  const start = Date.now();
  for (let attempt = 0; ; attempt++) {
    const res = http.post(TARGET + path, body, req);
    if (res.status === 200) {
      accepted.add(1);
      return;
    }
    if ((Date.now() - start) / 1000 > GIVE_UP_S) {
      console.error(`gave up: i=${i} status=${res.status} error="${res.error}"`);
      gaveUp.add(1);
      return;
    }
    retries.add(1);
    // Exponential backoff from 250 ms, capped at 4 s, with jitter so retries don't stampede.
    sleep(Math.min(4, 0.25 * 2 ** attempt) * (0.5 + Math.random() / 2));
  }
}

export function handleSummary(data) {
  const m = data.metrics;
  const v = {
    ITERATIONS: m.iterations ? m.iterations.values.count : 0,
    ACCEPTED: m.accepted ? m.accepted.values.count : 0,
    GAVE_UP: m.gave_up ? m.gave_up.values.count : 0,
    RETRIES: m.retries ? m.retries.values.count : 0,
    DROPPED: m.dropped_iterations ? m.dropped_iterations.values.count : 0,
  };
  const env = Object.entries(v).map(([k, val]) => `${k}=${val}`).join('\n') + '\n';
  return {
    stdout: `k6: ${v.ITERATIONS} webhooks, ${v.ACCEPTED} accepted, ${v.GAVE_UP} gave up, ${v.RETRIES} retries\n`,
    [SUMMARY_FILE]: env,
  };
}
