const API_URL = import.meta.env.VITE_API_URL || "";

function getWebSocketBaseUrl() {
  if (API_URL.startsWith("http://")) {
    return API_URL.replace(/^http:/, "ws:");
  }
  if (API_URL.startsWith("https://")) {
    return API_URL.replace(/^https:/, "wss:");
  }
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}`;
}

async function request(path, { method = "GET", body, token } = {}) {
  const response = await fetch(`${API_URL}${path}`, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {})
    },
    ...(body ? { body: JSON.stringify(body) } : {})
  });

  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(data.error || "Request failed");
  }

  return data;
}

function parseFilenameFromDisposition(disposition) {
  if (!disposition) return null;
  const utf8Match = disposition.match(/filename\*=UTF-8''([^;]+)/i);
  if (utf8Match?.[1]) {
    try {
      return decodeURIComponent(utf8Match[1]);
    } catch {
      return utf8Match[1];
    }
  }
  const plainMatch = disposition.match(/filename="?([^";]+)"?/i);
  return plainMatch?.[1] || null;
}

async function requestFile(path, { method = "GET", token } = {}) {
  const response = await fetch(`${API_URL}${path}`, {
    method,
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {})
    }
  });

  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw new Error(data.error || "Request failed");
  }

  return {
    blob: await response.blob(),
    filename: parseFilenameFromDisposition(response.headers.get("Content-Disposition"))
  };
}

export const api = {
  login: (payload) => request("/api/auth/login", { method: "POST", body: payload }),
  register: (payload) => request("/api/auth/register", { method: "POST", body: payload }),
  me: (token) => request("/api/me", { token }),
  wallet: (token) => request("/api/wallet", { token }),
  deposit: (token, amount) => request("/api/wallet/deposit", { method: "POST", token, body: { amount } }),
  withdraw: (token, amount) => request("/api/wallet/withdraw", { method: "POST", token, body: { amount } }),
  draws: (token) => request("/api/draws", { token }),
  buyTicket: (token, drawId, count) =>
    request(`/api/draws/${drawId}/tickets/buy`, { method: "POST", token, body: { count } }),
  myTickets: (token) => request("/api/my/tickets", { token }),
  notifications: (token) => request("/api/notifications", { token }),
  createDraw: (token, payload) => request("/api/draws/admin/create", { method: "POST", token, body: payload }),
  startDraw: (token, drawId) => request(`/api/draws/${drawId}/admin/start`, { method: "POST", token }),
  nextNumber: (token, drawId) => request(`/api/draws/${drawId}/admin/next-number`, { method: "POST", token }),
  finishDraw: (token, drawId) => request(`/api/draws/${drawId}/admin/finish`, { method: "POST", token }),
  reports: (token) => request("/api/admin/reports", { token }),
  reportsPdf: (token) => requestFile("/api/admin/reports/pdf", { token })
};

export function createDrawStream(onMessage, onStatus) {
  const wsUrlBase = getWebSocketBaseUrl();
  let socket;
  let stopped = false;
  let reconnectDelayMs = 700;

  function connect() {
    if (stopped) return;
    onStatus?.("connecting");
    socket = new WebSocket(`${wsUrlBase}/ws`);

    socket.addEventListener("open", () => {
      reconnectDelayMs = 700;
      onStatus?.("connected");
    });

    socket.addEventListener("message", (event) => {
      try {
        onMessage(JSON.parse(event.data));
      } catch {
        // ignore bad payloads
      }
    });

    socket.addEventListener("close", () => {
      if (stopped) return;
      onStatus?.("disconnected");
      setTimeout(connect, reconnectDelayMs);
      reconnectDelayMs = Math.min(reconnectDelayMs * 2, 5000);
    });

    socket.addEventListener("error", () => {
      onStatus?.("disconnected");
    });
  }

  connect();

  return {
    close() {
      stopped = true;
      if (socket) socket.close();
    }
  };
}
