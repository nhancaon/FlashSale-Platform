// Flash sale scenario: many users race for a small stock through gateway -> order -> inventory -> Oracle.
//
//   BASE_URL       gateway (default http://gateway:8080, i.e. run k6 on the compose network)
//   SKU            product on sale (stock is prepared by loadtest/run-benchmark.sh, default stock 100)
//   DEMO_PASSWORD  password of the simulated identity provider
//   USERS          distinct users, each VU is one user (default 1000)
//   VUS, RAMP, HOLD  load shape (default 1000 VUs, ramp 20s, hold 40s)
//
// Behaviour of one user: place an order for 1 unit; in 15% of the cases retry the same request at once with the same
// Idempotency-Key (a timeout or double click): it must return the stored result, never a second order.
// After "sold out" the user thinks for 1-3 s before trying again (people give up and refresh).
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate } from 'k6/metrics';
import exec from 'k6/execution';

const BASE = __ENV.BASE_URL || 'http://gateway:8080';
const SKU = __ENV.SKU || 'SKU-IPHONE';
const PASSWORD = __ENV.DEMO_PASSWORD;
const USERS = parseInt(__ENV.USERS || '1000', 10);
const VUS = parseInt(__ENV.VUS || '1000', 10);
const RUN = __ENV.RUN_ID || `${Date.now()}`;
const PREFIX = __ENV.USER_PREFIX || 'bench';

const created = new Counter('orders_created');           // 201
const replayed = new Counter('orders_replayed');         // 200 (idempotent replay)
const soldOut = new Counter('orders_sold_out');          // 409 OUT_OF_STOCK
const throttled = new Counter('orders_throttled');       // 429 from the gateway
const inProgress = new Counter('orders_in_progress');    // 409 REQUEST_IN_PROGRESS
const unexpected = new Counter('orders_unexpected');     // anything else (5xx, network errors, bad bodies)
const unexpectedRate = new Rate('unexpected_rate');

// 409 (business outcome) and 429 (throttling) are normal here, not failures.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 409, 429));

export const options = {
  scenarios: {
    flashsale: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: __ENV.RAMP || '20s', target: VUS },
        { duration: __ENV.HOLD || '40s', target: VUS },
        { duration: '10s', target: 0 },
      ],
      gracefulRampDown: '20s',
    },
  },
  thresholds: {
    // The sale is decided by correctness (checked by the reconcile script) and by not erroring or timing out.
    unexpected_rate: ['rate<0.01'],
    'http_req_duration{name:POST /api/orders}': ['p(95)<3000'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  setupTimeout: '180s',
};

export function setup() {
  const tokens = [];
  const headers = { headers: { 'Content-Type': 'application/json' }, tags: { name: 'POST /auth/login' } };
  for (let start = 0; start < USERS; start += 50) {
    const batch = [];
    for (let i = start; i < Math.min(start + 50, USERS); i++) {
      batch.push(['POST', `${BASE}/auth/login`, JSON.stringify({ username: `${PREFIX}-${i}`, password: PASSWORD }), headers]);
    }
    for (const res of http.batch(batch)) {
      if (res.status !== 200) {
        throw new Error(`login failed (${res.status}): ${res.body}`);
      }
      tokens.push(res.json('accessToken'));
    }
  }
  return { tokens };
}

let seq = 0;

export default function (data) {
  const vu = exec.vu.idInTest;
  const token = data.tokens[(vu - 1) % data.tokens.length];
  const key = `${RUN}-${vu}-${seq++}`;
  const params = {
    headers: { Authorization: `Bearer ${token}`, 'Idempotency-Key': key, 'Content-Type': 'application/json' },
    tags: { name: 'POST /api/orders' },
  };
  const body = JSON.stringify({ items: [{ sku: SKU, qty: 1 }] });

  const res = http.post(`${BASE}/api/orders`, body, params);
  const outcome = classify(res, 'first');

  if (Math.random() < 0.15) {
    // Same key again, right away: must be answered from the stored result (200) or "still running" (409 in progress).
    const again = http.post(`${BASE}/api/orders`, body, params);
    const ok = check(again, { 'retry with the same key is a replay or in progress': (r) => r.status === 200 || r.status === 409 });
    if (ok && again.status === 409 && again.json('code') !== 'REQUEST_IN_PROGRESS') {
      unexpected.add(1);
      unexpectedRate.add(1);
    }
    classify(again, 'retry');
  }

  sleep(outcome === 'sold_out' ? 1 + Math.random() * 2 : 0.2 + Math.random() * 0.6);
}

function classify(res, phase) {
  let outcome = 'unexpected';
  if (res.status === 201) {
    created.add(1);
    outcome = 'created';
  } else if (res.status === 200) {
    replayed.add(1);
    outcome = 'replayed';
  } else if (res.status === 202) {
    outcome = 'pending';
  } else if (res.status === 429) {
    throttled.add(1);
    outcome = 'throttled';
  } else if (res.status === 409) {
    const code = res.json('code');
    if (code === 'OUT_OF_STOCK') {
      soldOut.add(1);
      outcome = 'sold_out';
    } else if (code === 'REQUEST_IN_PROGRESS') {
      inProgress.add(1);
      outcome = 'in_progress';
    }
  }
  const bad = outcome === 'unexpected';
  if (bad) {
    unexpected.add(1);
  }
  unexpectedRate.add(bad);
  return outcome;
}
