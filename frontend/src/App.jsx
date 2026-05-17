import React, { useEffect, useMemo, useState } from "react";
import { api, createDrawStream } from "./api";

const I18N = {
  ru: {
    tabs: {
      draws: "Тиражи",
      wallet: "Баланс",
      tickets: "Мои билеты",
      notifications: "Уведомления",
      admin: "Админ"
    },
    actions: {
      refresh: "Обновить",
      logout: "Выйти",
      login: "Войти",
      register: "Зарегистрироваться"
    },
    labels: {
      streamOnline: "онлайн",
      streamConnecting: "подключение",
      streamOffline: "оффлайн"
    }
  },
  en: {
    tabs: {
      draws: "Draws",
      wallet: "Wallet",
      tickets: "My tickets",
      notifications: "Notifications",
      admin: "Admin"
    },
    actions: {
      refresh: "Refresh",
      logout: "Logout",
      login: "Sign in",
      register: "Sign up"
    },
    labels: {
      streamOnline: "online",
      streamConnecting: "connecting",
      streamOffline: "offline"
    }
  }
};

function sortTicketsBySerialDesc(tickets) {
  return [...(tickets || [])].sort((a, b) => Number(b.serialNumber || 0) - Number(a.serialNumber || 0));
}

function asArray(value) {
  return Array.isArray(value) ? value : [];
}

function toSafeNumber(value, fallback = 0) {
  const num = Number(value);
  return Number.isFinite(num) ? num : fallback;
}

function normalizeWallet(wallet) {
  return {
    ...(wallet && typeof wallet === "object" ? wallet : {}),
    balance: toSafeNumber(wallet?.balance, 0)
  };
}

function normalizeAdminSettings(settings) {
  return {
    lottoDrawnNumbersCount: toSafeNumber(settings?.lottoDrawnNumbersCount, 18),
    lottoBarrelsCount: toSafeNumber(settings?.lottoBarrelsCount, 36),
    lottoTicketNumbersCount: toSafeNumber(settings?.lottoTicketNumbersCount, 5),
    prizeBig: toSafeNumber(settings?.prizeBig, 5000),
    prizeMedium: toSafeNumber(settings?.prizeMedium, 1000),
    prizeSmall: toSafeNumber(settings?.prizeSmall, 100),
    notificationsRetentionDays: toSafeNumber(settings?.notificationsRetentionDays, 1825),
    auditRetentionDays: toSafeNumber(settings?.auditRetentionDays, 1825),
    retentionJobIntervalMin: toSafeNumber(settings?.retentionJobIntervalMin, 360),
    standardDrawIntervalMin: toSafeNumber(settings?.standardDrawIntervalMin, 1),
    standardDrawFutureCount: toSafeNumber(settings?.standardDrawFutureCount, 8)
  };
}

export default function App() {
  const defaultLang = (localStorage.getItem("lang") || "ru").toLowerCase();
  const [lang, setLang] = useState(defaultLang === "en" ? "en" : "ru");
  const [token, setToken] = useState(localStorage.getItem("token") || "");
  const [user, setUser] = useState(() => {
    const raw = localStorage.getItem("user");
    if (!raw) return null;
    try {
      return JSON.parse(raw);
    } catch {
      localStorage.removeItem("user");
      localStorage.removeItem("token");
      return null;
    }
  });

  const [authMode, setAuthMode] = useState("login");
  const [error, setError] = useState("");
  const [feed, setFeed] = useState([]);
  const [streamStatus, setStreamStatus] = useState("disconnected");
  const [activeTab, setActiveTab] = useState("draws");

  const [draws, setDraws] = useState([]);
  const [wallet, setWallet] = useState(normalizeWallet({ balance: 0 }));
  const [transactions, setTransactions] = useState([]);
  const [tickets, setTickets] = useState([]);
  const [notifications, setNotifications] = useState([]);
  const [report, setReport] = useState(null);
  const [adminSettings, setAdminSettings] = useState(null);
  const [settingsForm, setSettingsForm] = useState(normalizeAdminSettings(null));
  const [isSavingSettings, setIsSavingSettings] = useState(false);
  const [isDownloadingReport, setIsDownloadingReport] = useState(false);
  const [purchaseReceipt, setPurchaseReceipt] = useState(null);
  const [selectedTicket, setSelectedTicket] = useState(null);
  const [showInsufficientToast, setShowInsufficientToast] = useState(false);
  const [drawCreateToastMessage, setDrawCreateToastMessage] = useState("");

  const [authForm, setAuthForm] = useState({ email: "", password: "", name: "" });
  const [amount, setAmount] = useState(100);
  const [buyCounts, setBuyCounts] = useState({});
  const [drawForm, setDrawForm] = useState({
    name: "Вечерний тираж",
    drawAt: new Date(Date.now() + 3600000).toISOString().slice(0, 16),
    ticketPrice: 50
  });

  const isAdmin = user?.role === "admin";
  const i18n = I18N[lang] || I18N.ru;
  const tabs = [
    { id: "draws", title: i18n.tabs.draws },
    { id: "wallet", title: i18n.tabs.wallet },
    { id: "tickets", title: i18n.tabs.tickets },
    { id: "notifications", title: i18n.tabs.notifications }
  ];

  const nearestStandardDraw = useMemo(() => {
    const now = Date.now();
    return (
      draws
      .filter((draw) => draw.name?.startsWith("Стандартный тираж"))
      .filter((draw) => draw.status !== "finished")
      .filter((draw) => new Date(draw.drawAt).getTime() >= now)
      .sort((a, b) => new Date(a.drawAt).getTime() - new Date(b.drawAt).getTime())
      [0] || null
    );
  }, [draws]);

  const adminDraws = useMemo(() => {
    return draws
      .filter((draw) => !draw.name?.startsWith("Стандартный тираж"))
      .filter((draw) => draw.status !== "finished")
      .sort((a, b) => new Date(a.drawAt).getTime() - new Date(b.drawAt).getTime());
  }, [draws]);

  const drawsForGrid = useMemo(() => {
    const items = [];
    if (nearestStandardDraw) {
      items.push(nearestStandardDraw);
    }
    return items.concat(adminDraws);
  }, [nearestStandardDraw, adminDraws]);

  const liveDraw = useMemo(() => {
    let drawId = "";
    let status = "idle";
    let numbers = [];

    for (const event of feed) {
      if (event.type === "draw.number") {
        drawId = event.payload?.drawId || "";
        status = "running";
        numbers = event.payload?.winningNumbers || [];
        break;
      }
      if (event.type === "draw.started") {
        drawId = event.payload?.id || "";
        status = "running";
        numbers = event.payload?.winningNumbers || [];
        break;
      }
      if (event.type === "draw.finished") {
        drawId = event.payload?.id || "";
        status = "finished";
        numbers = event.payload?.winningNumbers || [];
        break;
      }
    }

    const drawFromList = draws.find((draw) => draw.id === drawId) || draws.find((draw) => draw.status === "running");
    if (!drawFromList) {
      return {
        title: "Ожидание следующего тиража",
        status,
        numbers
      };
    }

    const fallbackNumbers = drawFromList.winningNumbers || [];
    return {
      title: drawFromList.name,
      status: drawFromList.status === "running" ? "running" : status,
      numbers: numbers.length > 0 ? numbers : fallbackNumbers
    };
  }, [draws, feed]);

  function getMatchedNumbers(ticket) {
    const drawNumbers = asArray(ticket.draw?.winningNumbers);
    if (drawNumbers.length === 0) return new Set();
    const matched = asArray(ticket.numbers).filter((number) => drawNumbers.includes(number));
    return new Set(matched);
  }

  function applyDrawToTickets(drawId, winningNumbers, drawStatus) {
    if (!drawId) return;
    const normalizedNumbers = Array.isArray(winningNumbers) ? winningNumbers : [];

    setTickets((prev) =>
      prev.map((ticket) => {
        if (ticket.drawId !== drawId) return ticket;
        return {
          ...ticket,
          draw: {
            ...(ticket.draw || {}),
            id: ticket.draw?.id || drawId,
            winningNumbers: normalizedNumbers,
            ...(drawStatus ? { status: drawStatus } : {})
          }
        };
      })
    );

    setSelectedTicket((prev) => {
      if (!prev || prev.drawId !== drawId) return prev;
      return {
        ...prev,
        draw: {
          ...(prev.draw || {}),
          id: prev.draw?.id || drawId,
          winningNumbers: normalizedNumbers,
          ...(drawStatus ? { status: drawStatus } : {})
        }
      };
    });
  }

  async function refreshTicketsAndWallet() {
    try {
      const [ticketsData, walletData, notificationsData] = await Promise.all([
        api.myTickets(token),
        api.wallet(token),
        api.notifications(token)
      ]);
      setTickets(sortTicketsBySerialDesc(asArray(ticketsData?.tickets)));
      setWallet(normalizeWallet(walletData?.wallet));
      setTransactions(asArray(walletData?.transactions));
      setNotifications(asArray(notificationsData?.notifications));
    } catch {
      // keep current state if background refresh failed
    }
  }

  useEffect(() => {
    if (!token) return;

    let socket;
    try {
      socket = createDrawStream(
        (message) => {
          setFeed((prev) => [message, ...prev].slice(0, 40));

          if (message.type === "draw.started") {
            const draw = message.payload || {};
            setDraws((prev) =>
              prev.map((item) =>
                item.id === draw.id
                  ? {
                      ...item,
                      ...draw,
                      status: "running",
                      winningNumbers: draw.winningNumbers || item.winningNumbers || []
                    }
                  : item
              )
            );
            applyDrawToTickets(draw.id, draw.winningNumbers || [], "running");
          }

          if (message.type === "draw.number") {
            const payload = message.payload || {};
            setDraws((prev) =>
              prev.map((item) =>
                item.id === payload.drawId
                  ? {
                      ...item,
                      status: "running",
                      winningNumbers: payload.winningNumbers || item.winningNumbers || []
                    }
                  : item
              )
            );
            applyDrawToTickets(payload.drawId, payload.winningNumbers || [], "running");
          }

          if (message.type === "draw.finished") {
            const draw = message.payload || {};
            setDraws((prev) =>
              prev.map((item) =>
                item.id === draw.id
                  ? {
                      ...item,
                      ...draw,
                      status: "finished",
                      winningNumbers: draw.winningNumbers || item.winningNumbers || []
                    }
                  : item
              )
            );
            applyDrawToTickets(draw.id, draw.winningNumbers || [], "finished");
            void refreshTicketsAndWallet();
          }
        },
        (status) => setStreamStatus(status)
      );
    } catch {
      // ignore websocket errors
    }

    return () => {
      if (socket) socket.close();
      setStreamStatus("disconnected");
    };
  }, [token]);

  useEffect(() => {
    if (!token) return;
    refreshAll();
  }, [token]);

  useEffect(() => {
    if (!token || !isAdmin) return;
    loadAdminSettings();
  }, [token, isAdmin]);

  useEffect(() => {
    if (!token) return;
    const id = setInterval(() => {
      refreshAll();
    }, 15000);
    return () => clearInterval(id);
  }, [token, isAdmin]);

  useEffect(() => {
    if (error !== "Insufficient balance") return;
    setShowInsufficientToast(true);
    const id = setTimeout(() => setShowInsufficientToast(false), 3500);
    return () => clearTimeout(id);
  }, [error]);

  useEffect(() => {
    if (!drawCreateToastMessage) return;
    const id = setTimeout(() => setDrawCreateToastMessage(""), 5000);
    return () => clearTimeout(id);
  }, [drawCreateToastMessage]);

  async function refreshAll() {
    try {
      const [drawsData, walletData, ticketsData, notificationsData] = await Promise.all([
        api.draws(token),
        api.wallet(token),
        api.myTickets(token),
        api.notifications(token)
      ]);

      setDraws(asArray(drawsData?.draws));
      setWallet(normalizeWallet(walletData?.wallet));
      setTransactions(asArray(walletData?.transactions));
      setTickets(sortTicketsBySerialDesc(asArray(ticketsData?.tickets)));
      setNotifications(asArray(notificationsData?.notifications));

      if (isAdmin) {
        const reportData = await api.reports(token);
        setReport(reportData);
      }
    } catch (err) {
      setError(err.message);
    }
  }

  async function loadAdminSettings() {
    try {
      const response = await api.adminSettings(token);
      const normalized = normalizeAdminSettings(response?.settings);
      setAdminSettings(normalized);
      setSettingsForm(normalized);
    } catch (err) {
      setError(err.message);
    }
  }

  async function submitAuth(event) {
    event.preventDefault();
    setError("");
    try {
      const result =
        authMode === "login"
          ? await api.login({ email: authForm.email, password: authForm.password })
          : await api.register(authForm);

      localStorage.setItem("token", result.token);
      localStorage.setItem("user", JSON.stringify(result.user));
      setToken(result.token);
      setUser(result.user);
    } catch (err) {
      setError(err.message);
    }
  }

  function logout() {
    localStorage.removeItem("token");
    localStorage.removeItem("user");
    setToken("");
    setUser(null);
    setDraws([]);
    setTickets([]);
    setNotifications([]);
    setTransactions([]);
    setReport(null);
    setAdminSettings(null);
    setSettingsForm(normalizeAdminSettings(null));
    setFeed([]);
    setPurchaseReceipt(null);
    setSelectedTicket(null);
  }

  async function doDeposit() {
    try {
      await api.deposit(token, Number(amount));
      await refreshAll();
    } catch (err) {
      setError(err.message);
    }
  }

  async function doWithdraw() {
    try {
      await api.withdraw(token, Number(amount));
      await refreshAll();
    } catch (err) {
      setError(err.message);
    }
  }

  async function doBuyTicket(draw) {
    try {
      const rawCount = buyCounts[draw.id];
      if (rawCount === "") {
        setError("Введите количество билетов перед покупкой");
        return;
      }

      const count = rawCount == null ? 1 : Number(rawCount);
      if (!Number.isInteger(count) || count < 1) {
        setError("Введите корректное количество билетов (целое число от 1)");
        return;
      }
      const result = await api.buyTicket(token, draw.id, count);
      setBuyCounts((prev) => ({ ...prev, [draw.id]: 1 }));
      setPurchaseReceipt(asArray(result?.tickets));
      await refreshAll();
    } catch (err) {
      setError(err.message);
    }
  }

  async function doCreateDraw(event) {
    event.preventDefault();
    try {
      setDrawCreateToastMessage("");
      await api.createDraw(token, {
        ...drawForm,
        drawAt: new Date(drawForm.drawAt).toISOString()
      });
      await refreshAll();
    } catch (err) {
      const message = err?.message || "Не удалось создать тираж";
      setError(message);
      setDrawCreateToastMessage(`Создать тираж не удалось: ${message}`);
    }
  }

  async function downloadReportPdf() {
    try {
      setError("");
      setIsDownloadingReport(true);
      const result = await api.reportsPdf(token);
      const fallbackName = `admin-report-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-")}.pdf`;
      const filename = result?.filename || fallbackName;
      const blobUrl = URL.createObjectURL(result.blob);
      const link = document.createElement("a");
      link.href = blobUrl;
      link.download = filename;
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      URL.revokeObjectURL(blobUrl);
    } catch (err) {
      setError(err.message);
    } finally {
      setIsDownloadingReport(false);
    }
  }

  async function saveAdminSettings(event) {
    event.preventDefault();
    try {
      setError("");
      setIsSavingSettings(true);
      const payload = {
        lottoDrawnNumbersCount: Number(settingsForm.lottoDrawnNumbersCount),
        lottoBarrelsCount: Number(settingsForm.lottoBarrelsCount),
        lottoTicketNumbersCount: Number(settingsForm.lottoTicketNumbersCount),
        prizeBig: Number(settingsForm.prizeBig),
        prizeMedium: Number(settingsForm.prizeMedium),
        prizeSmall: Number(settingsForm.prizeSmall),
        notificationsRetentionDays: Number(settingsForm.notificationsRetentionDays),
        auditRetentionDays: Number(settingsForm.auditRetentionDays),
        retentionJobIntervalMin: Number(settingsForm.retentionJobIntervalMin),
        standardDrawIntervalMin: Number(settingsForm.standardDrawIntervalMin),
        standardDrawFutureCount: Number(settingsForm.standardDrawFutureCount)
      };

      const result = await api.updateAdminSettings(token, payload);
      const normalized = normalizeAdminSettings(result?.settings);
      setAdminSettings(normalized);
      setSettingsForm(normalized);
      await refreshAll();
    } catch (err) {
      setError(err.message);
    } finally {
      setIsSavingSettings(false);
    }
  }

  if (!token || !user) {
    return (
      <div className="auth-page">
        <div className="auth-card">
          <h1>Loto</h1>
          <p>Онлайн-лото с фейковым балансом и тиражами.</p>
          <form onSubmit={submitAuth}>
            {authMode === "register" && (
              <input
                placeholder="Имя"
                value={authForm.name}
                onChange={(e) => setAuthForm((prev) => ({ ...prev, name: e.target.value }))}
              />
            )}
            <input
              placeholder="Email"
              value={authForm.email}
              onChange={(e) => setAuthForm((prev) => ({ ...prev, email: e.target.value }))}
            />
            <input
              type="password"
              placeholder="Пароль"
              value={authForm.password}
              onChange={(e) => setAuthForm((prev) => ({ ...prev, password: e.target.value }))}
            />
            <button type="submit">{authMode === "login" ? i18n.actions.login : i18n.actions.register}</button>
          </form>
          <button className="link-btn" onClick={() => setAuthMode(authMode === "login" ? "register" : "login")}>
            {authMode === "login" ? "Нет аккаунта? Регистрация" : "Есть аккаунт? Вход"}
          </button>
          <p className="hint">Тест-админ: admin@loto.local / admin123</p>
          {error && <p className="error">{error}</p>}
        </div>
      </div>
    );
  }

  return (
    <div className="app-shell">
      <header>
        <div>
          <h1>Loto</h1>
          <p>{user.name} ({user.role})</p>
        </div>
        <div className="header-buttons">
          <button
            className="lang-toggle"
            onClick={() => {
              const next = lang === "ru" ? "en" : "ru";
              setLang(next);
              localStorage.setItem("lang", next);
            }}
          >
            {lang === "ru" ? "RU / EN" : "EN / RU"}
          </button>
          <button onClick={logout}>{i18n.actions.logout}</button>
        </div>
      </header>

      {error && error !== "Insufficient balance" && <p className="error">{error}</p>}

      <nav>
        {tabs.map((tab) => (
          <button key={tab.id} className={activeTab === tab.id ? "active" : ""} onClick={() => setActiveTab(tab.id)}>
            {tab.title}
          </button>
        ))}
        {isAdmin && (
          <button className={activeTab === "admin" ? "active" : ""} onClick={() => setActiveTab("admin")}>{i18n.tabs.admin}</button>
        )}
      </nav>

      <main>
        {activeTab === "draws" && (
          <section>
            <h2>Тиражи</h2>
            <div className="live-draw-widget">
              <div className="live-draw-widget__header">
                <h3>Live-розыгрыш</h3>
                <span>
                  Поток: {streamStatus === "connected" ? i18n.labels.streamOnline : streamStatus === "connecting" ? i18n.labels.streamConnecting : i18n.labels.streamOffline}
                </span>
              </div>
              <p>{liveDraw.title}</p>
              <p>Статус: {liveDraw.status === "running" ? "идёт розыгрыш" : liveDraw.status === "finished" ? "завершён" : "ожидание"}</p>
              <div className="live-draw-widget__numbers">
                {liveDraw.numbers.length > 0 ? (
                  liveDraw.numbers.map((number) => <span key={number}>{number}</span>)
                ) : (
                  <span className="live-draw-widget__empty">Числа появятся здесь в реальном времени</span>
                )}
              </div>
            </div>
            {drawsForGrid.length === 0 && <p>Сейчас нет доступных тиражей. Обновление списка происходит автоматически.</p>}
            <div className="grid">
              {drawsForGrid.map((draw) => (
                (() => {
                  const winningNumbers = asArray(draw.winningNumbers);
                  return (
                <article key={draw.id}>
                  <h3>{draw.name}</h3>
                  <p>Время тиража: {new Date(draw.drawAt).toLocaleString()}</p>
                  <p>Статус: {draw.status}</p>
                  <p>Цена билета: {draw.ticketPrice}</p>
                  <p>Формат билета: 5 из {draw.numbersCount}</p>
                  <p>Выигрышные: {winningNumbers.join(", ") || "-"}</p>
                  {draw.status !== "finished" && (
                    <>
                      <input
                        type="number"
                        min="1"
                        step="1"
                        placeholder="Количество билетов"
                        value={buyCounts[draw.id] ?? 1}
                        onChange={(e) =>
                          setBuyCounts((prev) => ({
                            ...prev,
                            [draw.id]: e.target.value
                          }))
                        }
                      />
                      <button onClick={() => doBuyTicket(draw)}>Купить билеты</button>
                    </>
                  )}
                </article>
                  );
                })()
              ))}
            </div>
          </section>
        )}

        {activeTab === "wallet" && (
          <section>
            <h2>Баланс: {toSafeNumber(wallet?.balance).toFixed(2)}</h2>
            <div className="wallet-actions">
              <input type="number" value={amount} onChange={(e) => setAmount(e.target.value)} />
              <button onClick={doDeposit}>Пополнить</button>
              <button onClick={doWithdraw}>Снять выигрыш</button>
            </div>
            <h3>История операций</h3>
            <ul>
              {transactions.map((tx) => (
                <li key={tx.id}>
                  {new Date(tx.createdAt).toLocaleString()} | {tx.type} | {tx.amount} | остаток {tx.balanceAfter}
                </li>
              ))}
            </ul>
          </section>
        )}

        {activeTab === "tickets" && (
          <section>
            <h2>Мои билеты</h2>
            <div className="ticket-grid">
              {tickets.map((ticket) => (
                (() => {
                  const matched = getMatchedNumbers(ticket);
                  const drawNumbers = ticket.draw?.winningNumbers || [];
                  return (
                <article
                  key={ticket.id}
                  className="ticket-card"
                  role="button"
                  tabIndex={0}
                  onClick={() => setSelectedTicket(ticket)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      setSelectedTicket(ticket);
                    }
                  }}
                >
                  <div className="ticket-card__top">
                    <span>Билет</span>
                    <strong>#{ticket.serialNumber}</strong>
                  </div>
                  <p>Дата исполнения: {new Date(ticket.executedAt || ticket.createdAt).toLocaleString()}</p>
                  <p>Статус: {ticket.status}</p>
                  <p>Выигрыш: {ticket.winAmount}</p>
                  <div className="ticket-mini-balls">
                    {asArray(ticket.numbers).map((number) => (
                      <span key={number} className={matched.has(number) ? "matched-number" : ""}>{number}</span>
                    ))}
                  </div>
                  {drawNumbers.length > 0 && (
                    <>
                      <p>Числа розыгрыша ({drawNumbers.length}):</p>
                      <div className="draw-numbers-grid">
                        {drawNumbers.map((number, index) => (
                          <span key={`${ticket.id}-draw-${index}`} className={matched.has(number) ? "matched-number" : ""}>
                            {number}
                          </span>
                        ))}
                      </div>
                    </>
                  )}
                </article>
                  );
                })()
              ))}
            </div>
          </section>
        )}

        {activeTab === "notifications" && (
          <section>
            <h2>Уведомления</h2>
            <p>
              Статус live-потока: {streamStatus === "connected" ? "подключено" : streamStatus === "connecting" ? "подключение" : "отключено"}
            </p>
            <ul>
              {notifications.map((item) => (
                <li key={item.id}>{new Date(item.createdAt).toLocaleString()} | {item.message}</li>
              ))}
            </ul>
            <h3>Поток тиража (WebSocket)</h3>
            <ul>
              {feed.map((event, index) => (
                <li key={`${event.type}-${index}`}>{event.type}: {JSON.stringify(event.payload)}</li>
              ))}
            </ul>
          </section>
        )}

        {isAdmin && activeTab === "admin" && (
          <section>
            <h2>Администрирование</h2>

            <h3>Параметры системы</h3>
            <form className="admin-form admin-settings-form" onSubmit={saveAdminSettings}>
              <div className="admin-settings-grid">
                <label>
                  Выпавших чисел в тираже
                  <input
                    type="number"
                    min="3"
                    value={settingsForm.lottoDrawnNumbersCount}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, lottoDrawnNumbersCount: e.target.value }))}
                  />
                </label>
                <label>
                  Количество бочонков
                  <input
                    type="number"
                    min="5"
                    value={settingsForm.lottoBarrelsCount}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, lottoBarrelsCount: e.target.value }))}
                  />
                </label>
                <label>
                  Чисел в билете
                  <input
                    type="number"
                    min="3"
                    value={settingsForm.lottoTicketNumbersCount}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, lottoTicketNumbersCount: e.target.value }))}
                  />
                </label>
                <label>
                  Приз за максимум совпадений
                  <input
                    type="number"
                    min="0"
                    step="0.01"
                    value={settingsForm.prizeBig}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, prizeBig: e.target.value }))}
                  />
                </label>
                <label>
                  Приз за -1 совпадение
                  <input
                    type="number"
                    min="0"
                    step="0.01"
                    value={settingsForm.prizeMedium}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, prizeMedium: e.target.value }))}
                  />
                </label>
                <label>
                  Приз за -2 совпадения
                  <input
                    type="number"
                    min="0"
                    step="0.01"
                    value={settingsForm.prizeSmall}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, prizeSmall: e.target.value }))}
                  />
                </label>
                <label>
                  Хранение уведомлений (дней)
                  <input
                    type="number"
                    min="0"
                    value={settingsForm.notificationsRetentionDays}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, notificationsRetentionDays: e.target.value }))}
                  />
                </label>
                <label>
                  Хранение аудита (дней)
                  <input
                    type="number"
                    min="0"
                    value={settingsForm.auditRetentionDays}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, auditRetentionDays: e.target.value }))}
                  />
                </label>
                <label>
                  Интервал retention-задачи (мин)
                  <input
                    type="number"
                    min="1"
                    value={settingsForm.retentionJobIntervalMin}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, retentionJobIntervalMin: e.target.value }))}
                  />
                </label>
                <label>
                  Интервал стандартных тиражей (мин)
                  <input
                    type="number"
                    min="1"
                    value={settingsForm.standardDrawIntervalMin}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, standardDrawIntervalMin: e.target.value }))}
                  />
                </label>
                <label>
                  Количество будущих стандартных тиражей
                  <input
                    type="number"
                    min="1"
                    value={settingsForm.standardDrawFutureCount}
                    onChange={(e) => setSettingsForm((prev) => ({ ...prev, standardDrawFutureCount: e.target.value }))}
                  />
                </label>
              </div>
              <button type="submit" disabled={isSavingSettings}>
                {isSavingSettings ? "Сохранение..." : "Сохранить параметры"}
              </button>
            </form>

            <form className="admin-form" onSubmit={doCreateDraw}>
              <input
                placeholder="Название"
                value={drawForm.name}
                onChange={(e) => setDrawForm((prev) => ({ ...prev, name: e.target.value }))}
              />
              <input
                type="datetime-local"
                value={drawForm.drawAt}
                onChange={(e) => setDrawForm((prev) => ({ ...prev, drawAt: e.target.value }))}
              />
              <input
                type="number"
                placeholder="Цена"
                value={drawForm.ticketPrice}
                onChange={(e) => setDrawForm((prev) => ({ ...prev, ticketPrice: Number(e.target.value) }))}
              />
              <p className="hint">
                Текущий формат: билет {adminSettings?.lottoTicketNumbersCount || settingsForm.lottoTicketNumbersCount} из {adminSettings?.lottoBarrelsCount || settingsForm.lottoBarrelsCount},
                разыгрывается {adminSettings?.lottoDrawnNumbersCount || settingsForm.lottoDrawnNumbersCount} чисел.
              </p>
              <button type="submit">Создать тираж</button>
            </form>

            <h3>Созданные админом тиражи</h3>
            <p className="hint">Запуск и проведение тиражей выполняется автоматически по таймеру. Ручной старт отключен.</p>
            <div className="grid">
              {adminDraws.map((draw) => (
                <article key={draw.id}>
                  <h4>{draw.name}</h4>
                  <p>{draw.status}</p>
                  <p>{asArray(draw.winningNumbers).join(", ") || "без номеров"}</p>
                </article>
              ))}
            </div>

            {report && (
              <>
                <h3>KPI</h3>
                <div className="admin-report-actions">
                  <button onClick={downloadReportPdf} disabled={isDownloadingReport}>
                    {isDownloadingReport ? "Формируется PDF..." : "Скачать отчет в PDF"}
                  </button>
                </div>
                <p>
                  Продажи: {report.kpi.sales} | Выплаты: {report.kpi.payouts} | Маржа: {report.kpi.margin} |
                  Тиражей: {report.kpi.draws} | Билетов: {report.kpi.tickets}
                </p>
              </>
            )}
          </section>
        )}
      </main>

      {purchaseReceipt && (
        <div className="modal-backdrop" onClick={() => setPurchaseReceipt(null)}>
          <div className="modal-card purchase-modal" onClick={(event) => event.stopPropagation()}>
            <div className="modal-header">
              <h3>Билеты куплены</h3>
              <button className="ghost-button" onClick={() => setPurchaseReceipt(null)}>
                Закрыть
              </button>
            </div>
            <p>Серийные номера купленных билетов:</p>
            <div className="serial-list">
              {purchaseReceipt.map((ticket) => (
                <span key={ticket.id}>#{ticket.serialNumber}</span>
              ))}
            </div>
          </div>
        </div>
      )}

      {selectedTicket && (
        <div className="modal-backdrop" onClick={() => setSelectedTicket(null)}>
          <div className="modal-card lotto-ticket" onClick={(event) => event.stopPropagation()}>
            <div className="ticket-strip">
              <span>LOTTO</span>
              <span>#{selectedTicket.serialNumber}</span>
            </div>
            <h3>Счастливый билет</h3>
            <p className="ticket-time">Исполнен: {new Date(selectedTicket.executedAt || selectedTicket.createdAt).toLocaleString()}</p>
            <p className="ticket-time">Совпадений по розыгрышу: {getMatchedNumbers(selectedTicket).size}</p>
            <div className="ticket-numbers">
              {asArray(selectedTicket.numbers).map((number) => {
                const matched = getMatchedNumbers(selectedTicket);
                return (
                  <span key={number} className={matched.has(number) ? "matched-number" : ""}>{number}</span>
                );
              })}
            </div>
            {asArray(selectedTicket.draw?.winningNumbers).length > 0 && (
              <>
                <p className="ticket-time">Числа розыгрыша ({selectedTicket.draw.winningNumbers.length}):</p>
                <div className="draw-numbers-grid draw-numbers-grid--modal">
                  {asArray(selectedTicket.draw?.winningNumbers).map((number, index) => {
                    const matched = getMatchedNumbers(selectedTicket);
                    return (
                      <span key={`selected-draw-${index}`} className={matched.has(number) ? "matched-number" : ""}>{number}</span>
                    );
                  })}
                </div>
              </>
            )}
            <p className="ticket-message">На этом билете сгенерированы лотто-числа. Сохраняйте серийный номер для истории и проверки результатов.</p>
            <div className="ticket-footer">
              <span>Статус: {selectedTicket.status}</span>
              <span>Выигрыш: {selectedTicket.winAmount}</span>
            </div>
            <button className="ghost-button" onClick={() => setSelectedTicket(null)}>
              Закрыть билет
            </button>
          </div>
        </div>
      )}

      {showInsufficientToast && (
        <div className="floating-toast" role="alert">
          Недостаточно средств на балансе
        </div>
      )}

      {drawCreateToastMessage && (
        <div className="floating-toast floating-toast--error" role="alert">
          {drawCreateToastMessage}
        </div>
      )}
    </div>
  );
}
