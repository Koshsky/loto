import http from "k6/http";
import { check, sleep } from "k6";

const BASE_URL = __ENV.BASE_URL || "https://localhost";

export const options = {
  vus: 20,
  duration: "2m",
  thresholds: {
    http_req_failed: ["rate<0.02"],
    http_req_duration: ["p(95)<3000"],
  },
};

export default function () {
  const health = http.get(`${BASE_URL}/api/health`);
  check(health, {
    "health status 200": (r) => r.status === 200,
  });

  const draws = http.get(`${BASE_URL}/api/draws`);
  check(draws, {
    "draws status 200": (r) => r.status === 200,
  });

  sleep(1);
}
