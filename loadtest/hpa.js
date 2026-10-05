// CPU load on inventory in the k3s lab, to watch the HorizontalPodAutoscaler scale it (make lab-hpa).
// Reads the stock of one SKU through the inventory NodePort as fast as VUS virtual users can, for HOLD.
import http from 'k6/http';
import { check } from 'k6';

const target = __ENV.TARGET || 'http://172.30.0.11:30083';

export const options = {
  stages: [
    { duration: '20s', target: Number(__ENV.VUS || 100) },
    { duration: __ENV.HOLD || '3m', target: Number(__ENV.VUS || 100) },
    { duration: '10s', target: 0 },
  ],
  thresholds: { http_req_failed: ['rate<0.01'] },
};

export default function () {
  const res = http.get(`${target}/v1/inventory/SKU-HEADSET`);
  check(res, { 'stock read': (r) => r.status === 200 });
}
