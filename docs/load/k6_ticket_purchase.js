import http from "k6/http";
import { check, sleep } from "k6";

const BASE_URL = __ENV.BASE_URL || "https://localhost";
const EMAIL = __ENV.USER_EMAIL || "admin@loto.local";
const PASSWORD = __ENV.USER_PASSWORD || "admin123";

export const options = {
  vus: 30,
  duration: "3m",
  thresholds: {
    http_req_failed: ["rate<0.03"],
    http_req_duration: ["p(95)<3000"],
  },
};

function authToken() {
  const res = http.post(`${BASE_URL}/api/auth/login`, JSON.stringify({ email: EMAIL, password: PASSWORD }), {
    headers: { "Content-Type": "application/json" },
  });
  check(res, { "login status 200": (r) => r.status === 200 });
  const body = JSON.parse(res.body || "{}");
  return body.token || "";
}

export default function () {
  const token = authToken();
  if (!token) {
    sleep(1);
    return;
  }

  const drawsRes = http.get(`${BASE_URL}/api/draws`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  check(drawsRes, { "draws status 200": (r) => r.status === 200 });
  const drawsBody = JSON.parse(drawsRes.body || "{}");
  const draw = (drawsBody.draws || []).find((item) => item.status === "scheduled" || item.status === "running");
  if (!draw) {
    sleep(1);
    return;
  }

  const buyRes = http.post(
    `${BASE_URL}/api/draws/${draw.id}/tickets/buy`,
    JSON.stringify({ count: 1 }),
    {
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
    }
  );

  check(buyRes, {
    "buy status 201 or 400": (r) => r.status === 201 || r.status === 400,
  });

  sleep(1);
}
