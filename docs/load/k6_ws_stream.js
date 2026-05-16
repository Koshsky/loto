import ws from "k6/ws";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "https://localhost";
const WS_URL = BASE_URL.replace(/^https:/, "wss:").replace(/^http:/, "ws:");

export const options = {
  vus: 100,
  duration: "2m",
  thresholds: {
    checks: ["rate>0.95"],
  },
};

export default function () {
  const response = ws.connect(`${WS_URL}/ws`, {}, function (socket) {
    socket.on("open", () => {
      socket.setTimeout(() => {
        socket.close();
      }, 1000);
    });

    socket.on("message", () => {
      // keep alive stream observation
    });

    socket.on("error", () => {
      socket.close();
    });
  });

  check(response, {
    "ws status is 101": (r) => r && r.status === 101,
  });
}
