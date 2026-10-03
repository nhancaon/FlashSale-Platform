// Preliminary rate limiter benchmark: POST /v1/check against one service.
//   BASE_URL  service under test (default http://host.docker.internal:8081)
//   VUS, DURATION  load shape (default 100 VUs, 15s)
//   KEYS      distinct keys to spread load over (default 1000)
import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://host.docker.internal:8081';
const KEYS = parseInt(__ENV.KEYS || '1000', 10);

export const options = {
  vus: parseInt(__ENV.VUS || '100', 10),
  duration: __ENV.DURATION || '15s',
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<100'],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const params = { headers: { 'Content-Type': 'application/json' } };
const RUN = `${Date.now()}`;

export default function () {
  // Huge limit: we measure decision latency/throughput, not rejection.
  const key = `bench-${RUN}-${Math.floor(Math.random() * KEYS)}`;
  const res = http.post(`${BASE_URL}/v1/check`,
    JSON.stringify({ key, limit: 1000000, windowSec: 60 }), params);
  check(res, { 'status 200': (r) => r.status === 200, 'allowed': (r) => r.status === 200 && r.json('allowed') === true });
}
