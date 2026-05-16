# Loto

Онлайн-лотерея с live-розыгрышем по WebSocket.

Состав проекта:
- `backend` (Go): REST API, WebSocket, бизнес-логика тиражей и расчета выигрышей.
- `frontend` (React + Vite): пользовательский интерфейс и админ-панель.
- `db` (PostgreSQL): хранение пользователей, билетов, тиражей, баланса, уведомлений и аудита.
- `caddy` (reverse proxy): HTTPS, автоматические сертификаты, маршрутизация запросов.

## Возможности

- Регистрация и авторизация (JWT, роли user/admin).
- Баланс: пополнение, покупка билетов, вывод выигрыша.
- Управление тиражами (админ): старт, выпадение номеров, завершение.
- Автоматический расчет статуса билетов и суммы выигрыша.
- Live-события по WebSocket.
- Отчеты для администратора.

## Быстрый Старт

1. Создайте `.env` на основе примера:

```bash
cp .env.example .env
```

2. Запустите сервисы:

```bash
docker compose up --build
```

3. Откройте домен из `.env` через HTTPS.

Важно: для автоматических публичных TLS-сертификатов Caddy нужен реальный домен, указывающий на сервер. Для `localhost` Caddy поднимет локальный сертификат.

Состояние Caddy (сертификаты и конфиг ACME) сохраняется в папках `caddy/data` и `caddy/config` на хосте.

Снаружи публикуются только порты `80` и `443`. Остальные сервисы остаются во внутренней сети Docker.

## Конфигурация Через .env

Проект использует переменные окружения из файла `.env` (через Docker Compose).

Ключевые переменные:
- `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_PORT`
- `PORT`, `BACKEND_PORT`, `FRONTEND_PORT`
- `CADDY_DOMAIN`
- `LOTTO_DRAWN_NUMBERS_COUNT`
- `LOTTO_PRIZE_5_MATCHES`, `LOTTO_PRIZE_4_MATCHES`, `LOTTO_PRIZE_3_MATCHES`
- `JWT_SECRET`
- `DATABASE_URL`
- `VITE_API_URL`

Что настраивается через `.env`:
- `LOTTO_DRAWN_NUMBERS_COUNT` - сколько чисел разыгрывается в тираже.
- `LOTTO_PRIZE_5_MATCHES` - выигрыш за 5 совпадений.
- `LOTTO_PRIZE_4_MATCHES` - выигрыш за 4 совпадения.
- `LOTTO_PRIZE_3_MATCHES` - выигрыш за 3 совпадения.
- `5 из 36` остается фиксированным форматом билета.
- `CADDY_DOMAIN` - домен, по которому Caddy выдает сайт и автоматически получает TLS-сертификат.

Пример заполнения есть в `.env.example`.

## Инициализация БД

- Схема БД инициализируется файлом `db/init.sql`.
- Этот скрипт выполняется только при первом создании volume `pgdata`.
- Для полной переинициализации БД:

```bash
docker compose down -v
docker compose up --build
```

## Тестовый Админ

- Email: `admin@loto.local`
- Password: `admin123`

## Полезные Команды

```bash
# Запуск в фоне
docker compose up -d --build

# Остановка
docker compose down

# Остановка с удалением volume базы
docker compose down -v

# Логи backend
docker compose logs -f backend
```

## Мониторинг И Надежность

- Для `backend`, `frontend`, `caddy` включены `restart: unless-stopped` и `healthcheck` в `docker-compose.yml`.
- Базовый health endpoint: `GET /api/health`.
- Базовые runtime-метрики backend: `GET /api/metrics`.

`/api/metrics` отдает:
- `uptimeSec`
- агрегаты `requests`, `errors`, `errorRate`
- метрики по маршрутам: `avgLatencyMs`, `minLatencyMs`, `maxLatencyMs`, `lastStatus`

## Нагрузочное Тестирование

В проекте добавлен smoke-сценарий для k6: `docs/load/k6_api_smoke.js`.

Пример запуска:

```bash
# установка k6 (если не установлен)
# https://k6.io/docs/get-started/installation/

BASE_URL=https://loto.koshsky.ru k6 run docs/load/k6_api_smoke.js
```

Пороговые условия в сценарии:
- `http_req_failed < 2%`
- `http_req_duration p(95) < 3000ms`

## API (основные)

- `POST /api/auth/register`
- `POST /api/auth/login`
- `GET /api/wallet`
- `POST /api/wallet/deposit`
- `POST /api/wallet/withdraw`
- `GET /api/draws`
- `POST /api/draws/:drawId/tickets/buy`
- `GET /api/my/tickets`
- `GET /api/notifications`
- `POST /api/draws/admin/create`
- `POST /api/draws/:drawId/admin/start`
- `POST /api/draws/:drawId/admin/next-number`
- `POST /api/draws/:drawId/admin/finish`
- `GET /api/admin/reports`
