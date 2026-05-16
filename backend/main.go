package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

type Wallet struct {
	Balance   float64 `json:"balance"`
	UpdatedAt string  `json:"updatedAt"`
}

type Draw struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	DrawAt         string  `json:"drawAt"`
	TicketPrice    float64 `json:"ticketPrice"`
	MaxNumber      int     `json:"maxNumber"`
	NumbersCount   int     `json:"numbersCount"`
	Status         string  `json:"status"`
	WinningNumbers []int   `json:"winningNumbers"`
	CreatedAt      string  `json:"createdAt"`
	StartedAt      string  `json:"startedAt,omitempty"`
	FinishedAt     string  `json:"finishedAt,omitempty"`
}

type Ticket struct {
	ID           string  `json:"id"`
	SerialNumber int64   `json:"serialNumber"`
	UserID       string  `json:"userId"`
	DrawID       string  `json:"drawId"`
	Numbers      []int   `json:"numbers"`
	Status       string  `json:"status"`
	WinAmount    float64 `json:"winAmount"`
	CreatedAt    string  `json:"createdAt"`
	ExecutedAt   string  `json:"executedAt"`
}

type TicketWithDraw struct {
	Ticket
	Draw *Draw `json:"draw"`
}

type Transaction struct {
	ID           string         `json:"id"`
	UserID       string         `json:"userId"`
	Type         string         `json:"type"`
	Amount       float64        `json:"amount"`
	BalanceAfter float64        `json:"balanceAfter"`
	Meta         map[string]any `json:"meta"`
	CreatedAt    string         `json:"createdAt"`
}

type Notification struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	CreatedAt string `json:"createdAt"`
}

type AuditEvent struct {
	ID          string         `json:"id"`
	ActorUserID string         `json:"actorUserId"`
	Action      string         `json:"action"`
	Details     map[string]any `json:"details"`
	CreatedAt   string         `json:"createdAt"`
}

type Claims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Role  string `json:"role"`
	Name  string `json:"name"`
	jwt.RegisteredClaims
}

type Server struct {
	db          *sql.DB
	jwtSecret   []byte
	upgrader    websocket.Upgrader
	clientsMu   sync.Mutex
	clients     map[*websocket.Conn]struct{}
	autoDrawSem chan struct{}
	startedAt   time.Time
	metricsMu   sync.Mutex
	metrics     map[string]*routeMetrics
	totalReq    int64
	totalErr    int64
}

type routeMetrics struct {
	Requests       int64
	Errors         int64
	TotalLatencyMs float64
	MaxLatencyMs   float64
	MinLatencyMs   float64
	LastStatusCode int
	LastRequestAt  time.Time
}

const (
	standardDrawBaseName    = "Стандартный тираж"
	standardDrawIntervalMin = 1
	standardDrawFutureCount = 8
	lottoBarrelsCount       = 36
	lottoTicketNumbersCount = 5
)

var (
	lottoDrawnNumbersCount     = mustEnvInt("LOTTO_DRAWN_NUMBERS_COUNT", 18)
	prizeBig                   = mustEnvFloat("LOTTO_PRIZE_5_MATCHES", 5000.0)
	prizeMedium                = mustEnvFloat("LOTTO_PRIZE_4_MATCHES", 1000.0)
	prizeSmall                 = mustEnvFloat("LOTTO_PRIZE_3_MATCHES", 100.0)
	notificationsRetentionDays = mustEnvInt("RETENTION_NOTIFICATIONS_DAYS", 365*5)
	auditRetentionDays         = mustEnvInt("RETENTION_AUDIT_DAYS", 365*5)
	retentionJobIntervalMin    = mustEnvInt("RETENTION_JOB_INTERVAL_MIN", 360)
)

func mustEnvInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("invalid %s=%q, using fallback %d", name, raw, fallback)
		return fallback
	}
	return value
}

func mustEnvFloat(name string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		log.Printf("invalid %s=%q, using fallback %.2f", name, raw, fallback)
		return fallback
	}
	return value
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func toISO(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func newServer(db *sql.DB, secret string) *Server {
	return &Server{
		db:        db,
		jwtSecret: []byte(secret),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		clients:     make(map[*websocket.Conn]struct{}),
		autoDrawSem: make(chan struct{}, 1),
		startedAt:   time.Now().UTC(),
		metrics:     make(map[string]*routeMetrics),
	}
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(statusCode int) {
	sr.statusCode = statusCode
	sr.ResponseWriter.WriteHeader(statusCode)
}

func (sr *statusRecorder) Write(data []byte) (int, error) {
	if sr.statusCode == 0 {
		sr.statusCode = http.StatusOK
	}
	return sr.ResponseWriter.Write(data)
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := sr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func normalizeMetricPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		if strings.Count(part, "-") == 4 && len(part) >= 32 {
			parts[i] = ":id"
		}
	}
	if len(parts) == 1 && parts[0] == "" {
		return "/"
	}
	return "/" + strings.Join(parts, "/")
}

func (s *Server) withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		statusCode := recorder.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		latencyMs := time.Since(start).Seconds() * 1000
		metricPath := normalizeMetricPath(r.URL.Path)

		s.metricsMu.Lock()
		s.totalReq++
		if statusCode >= 400 {
			s.totalErr++
		}
		entry, exists := s.metrics[metricPath]
		if !exists {
			entry = &routeMetrics{MinLatencyMs: latencyMs}
			s.metrics[metricPath] = entry
		}
		entry.Requests++
		if statusCode >= 400 {
			entry.Errors++
		}
		entry.TotalLatencyMs += latencyMs
		if latencyMs > entry.MaxLatencyMs {
			entry.MaxLatencyMs = latencyMs
		}
		if !exists || latencyMs < entry.MinLatencyMs {
			entry.MinLatencyMs = latencyMs
		}
		entry.LastStatusCode = statusCode
		entry.LastRequestAt = time.Now().UTC()
		s.metricsMu.Unlock()
	})
}

func (s *Server) createToken(user User) (string, error) {
	claims := Claims{
		Sub:   user.ID,
		Email: user.Email,
		Role:  user.Role,
		Name:  user.Name,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(7 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.jwtSecret)
}

func (s *Server) parseToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func parseTimeFilter(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		t = t.UTC()
		return &t, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, fmt.Errorf("invalid datetime format")
	}
	t = t.UTC()
	return &t, nil
}

func parseHistoryFilters(values url.Values, defaultLimit, maxLimit int) (*time.Time, *time.Time, int, error) {
	from, err := parseTimeFilter(values.Get("from"))
	if err != nil {
		return nil, nil, 0, errors.New("invalid 'from' parameter")
	}
	to, err := parseTimeFilter(values.Get("to"))
	if err != nil {
		return nil, nil, 0, errors.New("invalid 'to' parameter")
	}
	if from != nil && to != nil && from.After(*to) {
		return nil, nil, 0, errors.New("'from' must be less than or equal to 'to'")
	}
	limit := defaultLimit
	if rawLimit := strings.TrimSpace(values.Get("limit")); rawLimit != "" {
		parsed, parseErr := strconv.Atoi(rawLimit)
		if parseErr != nil || parsed <= 0 {
			return nil, nil, 0, errors.New("invalid 'limit' parameter")
		}
		if parsed > maxLimit {
			parsed = maxLimit
		}
		limit = parsed
	}
	return from, to, limit, nil
}

func (s *Server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (s *Server) authUser(r *http.Request) (*User, error) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, errors.New("unauthorized")
	}
	claims, err := s.parseToken(strings.TrimPrefix(authHeader, "Bearer "))
	if err != nil {
		return nil, err
	}
	var u User
	var createdAt time.Time
	err = s.db.QueryRow(`
		SELECT id, email, name, role, created_at
		FROM users
		WHERE id = $1
	`, claims.Sub).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &createdAt)
	if err != nil {
		return nil, errors.New("unauthorized")
	}
	u.CreatedAt = toISO(createdAt)
	return &u, nil
}

func (s *Server) requireAdmin(r *http.Request) (*User, error) {
	u, err := s.authUser(r)
	if err != nil {
		return nil, err
	}
	if u.Role != "admin" {
		return nil, errors.New("forbidden")
	}
	return u, nil
}

func (s *Server) currentBalanceTx(ctx context.Context, tx *sql.Tx, userID string) (float64, error) {
	var balance float64
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(ROUND(SUM(amount), 2), 0)
		FROM wallet_transactions
		WHERE user_id = $1
	`, userID).Scan(&balance)
	return balance, err
}

func (s *Server) addTransactionTx(ctx context.Context, tx *sql.Tx, userID, txType string, amount float64, meta map[string]any) (Transaction, error) {
	metaJSON, _ := json.Marshal(meta)
	balance, err := s.currentBalanceTx(ctx, tx, userID)
	if err != nil {
		return Transaction{}, err
	}
	balance = round2(balance + amount)
	var createdAt time.Time
	transaction := Transaction{ID: uuid.NewString()}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO wallet_transactions (id, user_id, type, amount, balance_after, meta, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, clock_timestamp())
		RETURNING created_at
	`, transaction.ID, userID, txType, round2(amount), balance, string(metaJSON)).Scan(&createdAt)
	if err != nil {
		return Transaction{}, err
	}
	transaction.UserID = userID
	transaction.Type = txType
	transaction.Amount = round2(amount)
	transaction.BalanceAfter = balance
	transaction.Meta = meta
	transaction.CreatedAt = toISO(createdAt)
	return transaction, nil
}

func (s *Server) notifyTx(ctx context.Context, tx *sql.Tx, userID, nType, message string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notifications (id, user_id, type, message)
		VALUES ($1, $2, $3, $4)
	`, uuid.NewString(), userID, nType, message)
	return err
}

func (s *Server) auditTx(ctx context.Context, tx *sql.Tx, actorUserID, action string, details map[string]any) error {
	detailsJSON, _ := json.Marshal(details)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (id, actor_user_id, action, details)
		VALUES ($1, $2, $3, $4::jsonb)
	`, uuid.NewString(), actorUserID, action, string(detailsJSON))
	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"ts":        nowISO(),
		"uptimeSec": int(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	type routeSnapshot struct {
		Path          string  `json:"path"`
		Requests      int64   `json:"requests"`
		Errors        int64   `json:"errors"`
		ErrorRate     float64 `json:"errorRate"`
		AvgLatencyMs  float64 `json:"avgLatencyMs"`
		MinLatencyMs  float64 `json:"minLatencyMs"`
		MaxLatencyMs  float64 `json:"maxLatencyMs"`
		LastStatus    int     `json:"lastStatus"`
		LastRequestAt string  `json:"lastRequestAt,omitempty"`
	}

	s.metricsMu.Lock()
	totalReq := s.totalReq
	totalErr := s.totalErr
	routes := make([]routeSnapshot, 0, len(s.metrics))
	for path, m := range s.metrics {
		avgLatency := 0.0
		errorRate := 0.0
		if m.Requests > 0 {
			avgLatency = m.TotalLatencyMs / float64(m.Requests)
			errorRate = float64(m.Errors) / float64(m.Requests)
		}
		snapshot := routeSnapshot{
			Path:         path,
			Requests:     m.Requests,
			Errors:       m.Errors,
			ErrorRate:    round2(errorRate * 100),
			AvgLatencyMs: round2(avgLatency),
			MinLatencyMs: round2(m.MinLatencyMs),
			MaxLatencyMs: round2(m.MaxLatencyMs),
			LastStatus:   m.LastStatusCode,
		}
		if !m.LastRequestAt.IsZero() {
			snapshot.LastRequestAt = toISO(m.LastRequestAt)
		}
		routes = append(routes, snapshot)
	}
	s.metricsMu.Unlock()

	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Requests > routes[j].Requests
	})

	totalErrorRate := 0.0
	if totalReq > 0 {
		totalErrorRate = float64(totalErr) / float64(totalReq)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uptimeSec": int(time.Since(s.startedAt).Seconds()),
		"totals": map[string]any{
			"requests":  totalReq,
			"errors":    totalErr,
			"errorRate": round2(totalErrorRate * 100),
		},
		"routes": routes,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Email == "" || req.Password == "" || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email, password and name are required"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Cannot hash password"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer tx.Rollback()

	user := User{ID: uuid.NewString(), Email: strings.ToLower(strings.TrimSpace(req.Email)), Name: req.Name, Role: "user"}
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (id, email, password_hash, name, role)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`, user.ID, user.Email, string(hash), user.Name, user.Role).Scan(&createdAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "User already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	user.CreatedAt = toISO(createdAt)

	if _, err = s.addTransactionTx(ctx, tx, user.ID, "init", 0, map[string]any{"reason": "wallet_init"}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = s.auditTx(ctx, tx, user.ID, "auth.register", map[string]any{"email": user.Email}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}

	token, _ := s.createToken(user)
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "user": user})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email and password are required"})
		return
	}

	var id, email, passwordHash, name, role string
	var createdAt time.Time
	err := s.db.QueryRow(`
		SELECT id, email, password_hash, name, role, created_at
		FROM users
		WHERE email = $1
	`, strings.ToLower(strings.TrimSpace(req.Email))).Scan(&id, &email, &passwordHash, &name, &role, &createdAt)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid credentials"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid credentials"})
		return
	}

	user := User{ID: id, Email: email, Name: name, Role: role, CreatedAt: toISO(createdAt)}
	_, _ = s.db.Exec(`
		INSERT INTO audit_events (id, actor_user_id, action, details)
		VALUES ($1, $2, $3, $4::jsonb)
	`, uuid.NewString(), user.ID, "auth.login", `{}`)

	token, _ := s.createToken(user)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": user})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) handleWallet(w http.ResponseWriter, r *http.Request) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	from, to, limit, err := parseHistoryFilters(r.URL.Query(), 100, 500)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var wallet Wallet
	var updatedAt sql.NullTime
	err = s.db.QueryRow(`
		SELECT
			COALESCE(ROUND(SUM(amount), 2), 0) AS balance,
			MAX(created_at) AS updated_at
		FROM wallet_transactions
		WHERE user_id = $1
	`, user.ID).Scan(&wallet.Balance, &updatedAt)
	if err != nil {
		wallet = Wallet{Balance: 0, UpdatedAt: nowISO()}
	} else if updatedAt.Valid {
		wallet.UpdatedAt = toISO(updatedAt.Time)
	} else {
		wallet.UpdatedAt = nowISO()
	}

	args := []any{user.ID}
	filters := []string{"user_id = $1"}
	if from != nil {
		args = append(args, *from)
		filters = append(filters, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if to != nil {
		args = append(args, *to)
		filters = append(filters, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf(`
		SELECT id, user_id, type, amount, balance_after, meta, created_at
		FROM wallet_transactions
		WHERE %s
		ORDER BY created_at DESC, ctid DESC
		LIMIT $%d
	`, strings.Join(filters, " AND "), len(args))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer rows.Close()
	transactions := []Transaction{}
	for rows.Next() {
		var tx Transaction
		var metaRaw []byte
		var createdAt time.Time
		if err = rows.Scan(&tx.ID, &tx.UserID, &tx.Type, &tx.Amount, &tx.BalanceAfter, &metaRaw, &createdAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		tx.CreatedAt = toISO(createdAt)
		_ = json.Unmarshal(metaRaw, &tx.Meta)
		transactions = append(transactions, tx)
	}

	writeJSON(w, http.StatusOK, map[string]any{"wallet": wallet, "transactions": transactions})
}

func (s *Server) handleDeposit(w http.ResponseWriter, r *http.Request) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err = readJSON(r, &req); err != nil || req.Amount <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Positive amount is required"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer tx.Rollback()

	transaction, err := s.addTransactionTx(ctx, tx, user.ID, "deposit", req.Amount, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = s.notifyTx(ctx, tx, user.ID, "deposit", fmt.Sprintf("Пополнение на сумму %.2f", req.Amount)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = s.auditTx(ctx, tx, user.ID, "wallet.deposit", map[string]any{"amount": req.Amount}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"transaction": transaction})
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err = readJSON(r, &req); err != nil || req.Amount <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Positive amount is required"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer tx.Rollback()

	balance, err := s.currentBalanceTx(ctx, tx, user.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if balance < req.Amount {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Insufficient balance"})
		return
	}

	transaction, err := s.addTransactionTx(ctx, tx, user.ID, "withdraw", -req.Amount, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = s.notifyTx(ctx, tx, user.ID, "withdraw", fmt.Sprintf("Списание выигрыша на сумму %.2f", req.Amount)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = s.auditTx(ctx, tx, user.ID, "wallet.withdraw", map[string]any{"amount": req.Amount}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"transaction": transaction})
}

func validateNumbers(numbers []int, maxNumber, numbersCount int) error {
	if len(numbers) != numbersCount {
		return fmt.Errorf("Exactly %d numbers are required", numbersCount)
	}
	seen := map[int]bool{}
	for _, n := range numbers {
		if n < 1 || n > maxNumber {
			return fmt.Errorf("Numbers must be integers in range 1..%d", maxNumber)
		}
		if seen[n] {
			return errors.New("Numbers must be unique")
		}
		seen[n] = true
	}
	return nil
}

func calculateWin(matches int) float64 {
	if matches == lottoTicketNumbersCount {
		return prizeBig
	}
	if matches == lottoTicketNumbersCount-1 {
		return prizeMedium
	}
	if matches == lottoTicketNumbersCount-2 {
		return prizeSmall
	}
	return 0
}

func randomNumbers(maxNumber, numbersCount int) []int {
	selected := make(map[int]bool, numbersCount)
	result := make([]int, 0, numbersCount)
	for len(result) < numbersCount {
		n := rand.Intn(maxNumber) + 1
		if selected[n] {
			continue
		}
		selected[n] = true
		result = append(result, n)
	}
	sort.Ints(result)
	return result
}

func int64SliceToInt(values []int64) []int {
	result := make([]int, len(values))
	for i, value := range values {
		result[i] = int(value)
	}
	return result
}

func (s *Server) mapDraw(rows Scanner) (*Draw, error) {
	var d Draw
	var winningNumbersRaw []int64
	var drawAt, createdAt time.Time
	var startedAt, finishedAt sql.NullTime
	if err := rows.Scan(&d.ID, &d.Name, &drawAt, &d.TicketPrice, &d.MaxNumber, &d.NumbersCount, &d.Status, pq.Array(&winningNumbersRaw), &createdAt, &startedAt, &finishedAt); err != nil {
		return nil, err
	}
	d.WinningNumbers = int64SliceToInt(winningNumbersRaw)
	d.DrawAt = toISO(drawAt)
	d.CreatedAt = toISO(createdAt)
	if startedAt.Valid {
		d.StartedAt = toISO(startedAt.Time)
	}
	if finishedAt.Valid {
		d.FinishedAt = toISO(finishedAt.Time)
	}
	return &d, nil
}

type Scanner interface {
	Scan(dest ...any) error
}

func (s *Server) handleDraws(w http.ResponseWriter, r *http.Request) {
	_, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	from, to, limit, err := parseHistoryFilters(r.URL.Query(), 500, 2000)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	args := []any{}
	filters := []string{"TRUE"}
	if from != nil {
		args = append(args, *from)
		filters = append(filters, fmt.Sprintf("draw_at >= $%d", len(args)))
	}
	if to != nil {
		args = append(args, *to)
		filters = append(filters, fmt.Sprintf("draw_at <= $%d", len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf(`
		SELECT id, name, draw_at, ticket_price, max_number, numbers_count, status, winning_numbers, created_at, started_at, finished_at
		FROM draws
		WHERE %s
		ORDER BY created_at DESC
		LIMIT $%d
	`, strings.Join(filters, " AND "), len(args))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer rows.Close()
	draws := []Draw{}
	for rows.Next() {
		d, mapErr := s.mapDraw(rows)
		if mapErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		draws = append(draws, *d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"draws": draws})
}

func (s *Server) handleCreateDraw(w http.ResponseWriter, r *http.Request) {
	admin, err := s.requireAdmin(r)
	if err != nil {
		status := http.StatusUnauthorized
		if err.Error() == "forbidden" {
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]string{"error": "Forbidden"})
		return
	}
	var req struct {
		Name         string  `json:"name"`
		DrawAt       string  `json:"drawAt"`
		TicketPrice  float64 `json:"ticketPrice"`
		MaxNumber    int     `json:"maxNumber"`
		NumbersCount int     `json:"numbersCount"`
	}
	if err = readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Name == "" || req.DrawAt == "" || req.TicketPrice <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, drawAt and ticketPrice are required"})
		return
	}
	req.MaxNumber = lottoBarrelsCount
	req.NumbersCount = lottoDrawnNumbersCount
	drawAt, err := time.Parse(time.RFC3339, req.DrawAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drawAt must be ISO datetime"})
		return
	}
	if !drawAt.UTC().After(time.Now().UTC()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drawAt must be in the future"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer tx.Rollback()

	draw := Draw{ID: uuid.NewString()}
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO draws (id, name, draw_at, ticket_price, max_number, numbers_count, status, winning_numbers)
		VALUES ($1, $2, $3, $4, $5, $6, 'scheduled', $7)
		RETURNING created_at
	`, draw.ID, req.Name, drawAt.UTC(), round2(req.TicketPrice), req.MaxNumber, req.NumbersCount, pq.Array([]int{})).Scan(&createdAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = s.auditTx(ctx, tx, admin.ID, "draw.create", map[string]any{"drawId": draw.ID}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}

	draw.Name = req.Name
	draw.DrawAt = toISO(drawAt)
	draw.TicketPrice = round2(req.TicketPrice)
	draw.MaxNumber = req.MaxNumber
	draw.NumbersCount = req.NumbersCount
	draw.Status = "scheduled"
	draw.WinningNumbers = []int{}
	draw.CreatedAt = toISO(createdAt)
	writeJSON(w, http.StatusCreated, map[string]any{"draw": draw})
}

func (s *Server) handleBuyTicket(w http.ResponseWriter, r *http.Request, drawID string) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var req struct {
		Count int `json:"count"`
	}
	if err = readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Count <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "count must be a positive integer"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer tx.Rollback()

	var draw Draw
	var winningNumbersRaw []int64
	var drawAt, createdAt time.Time
	var startedAt, finishedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, draw_at, ticket_price, max_number, numbers_count, status, winning_numbers, created_at, started_at, finished_at
		FROM draws
		WHERE id = $1
		FOR UPDATE
	`, drawID).Scan(&draw.ID, &draw.Name, &drawAt, &draw.TicketPrice, &draw.MaxNumber, &draw.NumbersCount, &draw.Status, pq.Array(&winningNumbersRaw), &createdAt, &startedAt, &finishedAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Draw not found"})
		return
	}
	draw.WinningNumbers = int64SliceToInt(winningNumbersRaw)
	if draw.Status != "scheduled" && draw.Status != "running" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Draw is not available for ticket purchase"})
		return
	}
	totalPrice := round2(draw.TicketPrice * float64(req.Count))
	balance, err := s.currentBalanceTx(ctx, tx, user.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	if balance < totalPrice {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Insufficient balance"})
		return
	}

	createdTickets := make([]Ticket, 0, req.Count)
	usedInBatch := make(map[string]bool, req.Count)
	for i := 0; i < req.Count; i++ {
		var ticketNumbers []int
		var duplicateCount int
		for attempt := 0; attempt < 50; attempt++ {
			ticketNumbers = randomNumbers(lottoBarrelsCount, lottoTicketNumbersCount)
			key := fmt.Sprint(ticketNumbers)
			if usedInBatch[key] {
				continue
			}
			err = tx.QueryRowContext(ctx, `
				SELECT COUNT(*)
				FROM tickets
				WHERE user_id = $1 AND draw_id = $2 AND numbers = $3
			`, user.ID, drawID, pq.Array(ticketNumbers)).Scan(&duplicateCount)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
				return
			}
			if duplicateCount == 0 {
				usedInBatch[key] = true
				break
			}
		}
		if len(ticketNumbers) == 0 {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not generate unique ticket numbers"})
			return
		}
		createdTickets = append(createdTickets, Ticket{Numbers: ticketNumbers})
	}

	if _, err = s.addTransactionTx(ctx, tx, user.ID, "ticket_purchase", -totalPrice, map[string]any{"drawId": drawID, "count": req.Count}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	for i := range createdTickets {
		ticket := &createdTickets[i]
		ticket.ID = uuid.NewString()
		ticket.UserID = user.ID
		ticket.DrawID = drawID
		ticket.Status = "pending"
		ticket.WinAmount = 0
		ticket.ExecutedAt = nowISO()
		var tCreatedAt time.Time
		err = tx.QueryRowContext(ctx, `
			INSERT INTO tickets (id, user_id, draw_id, numbers, status, win_amount)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING serial_number, created_at
		`, ticket.ID, ticket.UserID, ticket.DrawID, pq.Array(ticket.Numbers), ticket.Status, ticket.WinAmount).Scan(&ticket.SerialNumber, &tCreatedAt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		ticket.CreatedAt = toISO(tCreatedAt)
		ticket.ExecutedAt = ticket.CreatedAt
	}

	_ = s.notifyTx(ctx, tx, user.ID, "ticket", fmt.Sprintf("Куплено %d билетов на тираж %s", req.Count, draw.Name))
	_ = s.auditTx(ctx, tx, user.ID, "ticket.buy", map[string]any{"drawId": drawID, "count": req.Count})
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	log.Printf("ticket purchase: user=%s draw=%s count=%d total=%.2f", user.ID, drawID, req.Count, totalPrice)

	writeJSON(w, http.StatusCreated, map[string]any{"tickets": createdTickets, "count": req.Count})
}

func (s *Server) handleMyTickets(w http.ResponseWriter, r *http.Request) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	from, to, limit, err := parseHistoryFilters(r.URL.Query(), 200, 1000)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	args := []any{user.ID}
	filters := []string{"t.user_id = $1"}
	if from != nil {
		args = append(args, *from)
		filters = append(filters, fmt.Sprintf("t.created_at >= $%d", len(args)))
	}
	if to != nil {
		args = append(args, *to)
		filters = append(filters, fmt.Sprintf("t.created_at <= $%d", len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf(`
		SELECT
			t.id,
			t.serial_number,
			t.user_id,
			t.draw_id,
			t.numbers,
			t.status,
			t.win_amount,
			t.created_at,
			d.id,
			d.name,
			d.draw_at,
			d.ticket_price,
			d.max_number,
			d.numbers_count,
			d.status,
			d.winning_numbers,
			d.created_at,
			d.started_at,
			d.finished_at
		FROM tickets t
		LEFT JOIN draws d ON d.id = t.draw_id
		WHERE %s
		ORDER BY t.serial_number DESC NULLS LAST, t.created_at DESC
		LIMIT $%d
	`, strings.Join(filters, " AND "), len(args))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer rows.Close()

	items := []TicketWithDraw{}
	for rows.Next() {
		var item TicketWithDraw
		var numbersRaw []int64
		var drawWinningNumbersRaw []int64
		var tCreatedAt time.Time

		var drawID sql.NullString
		var drawName sql.NullString
		var drawAt sql.NullTime
		var drawTicketPrice sql.NullFloat64
		var drawMaxNumber sql.NullInt64
		var drawNumbersCount sql.NullInt64
		var drawStatus sql.NullString
		var drawCreatedAt sql.NullTime
		var drawStartedAt sql.NullTime
		var drawFinishedAt sql.NullTime

		if err = rows.Scan(
			&item.ID,
			&item.SerialNumber,
			&item.UserID,
			&item.DrawID,
			pq.Array(&numbersRaw),
			&item.Status,
			&item.WinAmount,
			&tCreatedAt,
			&drawID,
			&drawName,
			&drawAt,
			&drawTicketPrice,
			&drawMaxNumber,
			&drawNumbersCount,
			&drawStatus,
			pq.Array(&drawWinningNumbersRaw),
			&drawCreatedAt,
			&drawStartedAt,
			&drawFinishedAt,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		item.Numbers = make([]int, len(numbersRaw))
		for i, number := range numbersRaw {
			item.Numbers[i] = int(number)
		}
		item.CreatedAt = toISO(tCreatedAt)
		item.ExecutedAt = item.CreatedAt

		if drawID.Valid {
			draw := &Draw{
				ID:             drawID.String,
				Name:           drawName.String,
				TicketPrice:    drawTicketPrice.Float64,
				MaxNumber:      int(drawMaxNumber.Int64),
				NumbersCount:   int(drawNumbersCount.Int64),
				Status:         drawStatus.String,
				WinningNumbers: int64SliceToInt(drawWinningNumbersRaw),
			}
			if drawAt.Valid {
				draw.DrawAt = toISO(drawAt.Time)
			}
			if drawCreatedAt.Valid {
				draw.CreatedAt = toISO(drawCreatedAt.Time)
			}
			if drawStartedAt.Valid {
				draw.StartedAt = toISO(drawStartedAt.Time)
			}
			if drawFinishedAt.Valid {
				draw.FinishedAt = toISO(drawFinishedAt.Time)
			}
			item.Draw = draw
		}

		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": items})
}

func (s *Server) broadcast(message map[string]any) {
	payload, _ := json.Marshal(message)
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for c := range s.clients {
		_ = c.WriteMessage(websocket.TextMessage, payload)
	}
}

func (s *Server) handleAdminDrawAction(w http.ResponseWriter, r *http.Request, drawID, action string) {
	admin, err := s.requireAdmin(r)
	if err != nil {
		status := http.StatusUnauthorized
		if err.Error() == "forbidden" {
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]string{"error": "Forbidden"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer tx.Rollback()

	var draw Draw
	var winningNumbersRaw []int64
	var drawAt, createdAt time.Time
	var startedAt, finishedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, draw_at, ticket_price, max_number, numbers_count, status, winning_numbers, created_at, started_at, finished_at
		FROM draws
		WHERE id = $1
		FOR UPDATE
	`, drawID).Scan(&draw.ID, &draw.Name, &drawAt, &draw.TicketPrice, &draw.MaxNumber, &draw.NumbersCount, &draw.Status, pq.Array(&winningNumbersRaw), &createdAt, &startedAt, &finishedAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Draw not found"})
		return
	}
	draw.WinningNumbers = int64SliceToInt(winningNumbersRaw)

	switch action {
	case "start":
		if draw.Status != "scheduled" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Draw cannot be started"})
			return
		}
		err = tx.QueryRowContext(ctx, `
			UPDATE draws
			SET status = 'running', started_at = NOW()
			WHERE id = $1
			RETURNING started_at
		`, drawID).Scan(&startedAt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		draw.Status = "running"
		draw.StartedAt = toISO(startedAt.Time)
		_ = s.auditTx(ctx, tx, admin.ID, "draw.start", map[string]any{"drawId": drawID})
		if err = tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		log.Printf("draw start: admin=%s draw=%s", admin.ID, drawID)
		s.broadcast(map[string]any{"type": "draw.started", "payload": draw})
		writeJSON(w, http.StatusOK, map[string]any{"draw": draw})
		return
	case "next-number":
		if draw.Status != "running" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Draw is not running"})
			return
		}
		if len(draw.WinningNumbers) >= lottoDrawnNumbersCount {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "All numbers already drawn"})
			return
		}
		pool := make([]int, 0)
		used := map[int]bool{}
		for _, n := range draw.WinningNumbers {
			used[n] = true
		}
		for i := 1; i <= lottoBarrelsCount; i++ {
			if !used[i] {
				pool = append(pool, i)
			}
		}
		nextNumber := pool[rand.Intn(len(pool))]
		draw.WinningNumbers = append(draw.WinningNumbers, nextNumber)
		_, err = tx.ExecContext(ctx, `
			UPDATE draws
			SET winning_numbers = $2
			WHERE id = $1
		`, drawID, pq.Array(draw.WinningNumbers))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		_ = s.auditTx(ctx, tx, admin.ID, "draw.nextNumber", map[string]any{"drawId": drawID, "number": nextNumber})
		if err = tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		log.Printf("draw next number: admin=%s draw=%s number=%d", admin.ID, drawID, nextNumber)
		s.broadcast(map[string]any{"type": "draw.number", "payload": map[string]any{"drawId": drawID, "number": nextNumber, "winningNumbers": draw.WinningNumbers}})
		writeJSON(w, http.StatusOK, map[string]any{"draw": draw, "number": nextNumber})
		return
	case "finish":
		if draw.Status != "running" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Draw is not running"})
			return
		}
		for len(draw.WinningNumbers) < lottoDrawnNumbersCount {
			pool := make([]int, 0)
			used := map[int]bool{}
			for _, n := range draw.WinningNumbers {
				used[n] = true
			}
			for i := 1; i <= lottoBarrelsCount; i++ {
				if !used[i] {
					pool = append(pool, i)
				}
			}
			num := pool[rand.Intn(len(pool))]
			draw.WinningNumbers = append(draw.WinningNumbers, num)
		}
		type settledTicket struct {
			ticketID string
			userID   string
			numbers  []int
		}

		rows, qErr := tx.QueryContext(ctx, `
			SELECT id, user_id, numbers
			FROM tickets
			WHERE draw_id = $1
			FOR UPDATE
		`, drawID)
		if qErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		toSettle := make([]settledTicket, 0)
		for rows.Next() {
			var ticketID, userID string
			var numbersRaw []int64
			if err = rows.Scan(&ticketID, &userID, pq.Array(&numbersRaw)); err != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
				return
			}
			numbers := make([]int, len(numbersRaw))
			for i, number := range numbersRaw {
				numbers[i] = int(number)
			}
			toSettle = append(toSettle, settledTicket{ticketID: ticketID, userID: userID, numbers: numbers})
		}
		rows.Close()

		for _, ticket := range toSettle {
			matches := 0
			for _, n := range ticket.numbers {
				for _, wn := range draw.WinningNumbers {
					if n == wn {
						matches++
					}
				}
			}
			win := calculateWin(matches)
			status := "lost"
			if win > 0 {
				status = "won"
			}
			if _, err = tx.ExecContext(ctx, `
				UPDATE tickets
				SET status = $2, win_amount = $3
				WHERE id = $1
			`, ticket.ticketID, status, win); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
				return
			}
			if win > 0 {
				if _, err = s.addTransactionTx(ctx, tx, ticket.userID, "winning_credit", win, map[string]any{"drawId": drawID, "ticketId": ticket.ticketID}); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
					return
				}
				_ = s.notifyTx(ctx, tx, ticket.userID, "winning", fmt.Sprintf("Выигрыш %.2f по тиражу %s", win, draw.Name))
			}
		}
		err = tx.QueryRowContext(ctx, `
			UPDATE draws
			SET status = 'finished', finished_at = NOW(), winning_numbers = $2
			WHERE id = $1
			RETURNING finished_at
		`, drawID, pq.Array(draw.WinningNumbers)).Scan(&finishedAt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		draw.Status = "finished"
		draw.FinishedAt = toISO(finishedAt.Time)
		_ = s.auditTx(ctx, tx, admin.ID, "draw.finish", map[string]any{"drawId": drawID})
		if err = tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		log.Printf("draw finish: admin=%s draw=%s winning_numbers=%d", admin.ID, drawID, len(draw.WinningNumbers))
		s.broadcast(map[string]any{"type": "draw.finished", "payload": draw})
		writeJSON(w, http.StatusOK, map[string]any{"draw": draw})
		return
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
	}
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	user, err := s.authUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	from, to, limit, err := parseHistoryFilters(r.URL.Query(), 100, 1000)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	args := []any{user.ID}
	filters := []string{"user_id = $1"}
	if from != nil {
		args = append(args, *from)
		filters = append(filters, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if to != nil {
		args = append(args, *to)
		filters = append(filters, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf(`
		SELECT id, user_id, type, message, created_at
		FROM notifications
		WHERE %s
		ORDER BY created_at DESC
		LIMIT $%d
	`, strings.Join(filters, " AND "), len(args))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer rows.Close()
	list := []Notification{}
	for rows.Next() {
		var n Notification
		var createdAt time.Time
		if err = rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Message, &createdAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		n.CreatedAt = toISO(createdAt)
		list = append(list, n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": list})
}

func (s *Server) handleAdminReports(w http.ResponseWriter, r *http.Request) {
	_, err := s.requireAdmin(r)
	if err != nil {
		status := http.StatusUnauthorized
		if err.Error() == "forbidden" {
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]string{"error": "Forbidden"})
		return
	}
	var sales, payouts float64
	_ = s.db.QueryRow(`
		SELECT COALESCE(SUM(-amount), 0)
		FROM wallet_transactions
		WHERE type = 'ticket_purchase'
	`).Scan(&sales)
	_ = s.db.QueryRow(`
		SELECT COALESCE(SUM(amount), 0)
		FROM wallet_transactions
		WHERE type = 'winning_credit'
	`).Scan(&payouts)

	var drawsCount, ticketsCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM draws`).Scan(&drawsCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM tickets`).Scan(&ticketsCount)

	rows, err := s.db.Query(`
		SELECT id, actor_user_id, action, details, created_at
		FROM audit_events
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer rows.Close()
	audit := []AuditEvent{}
	for rows.Next() {
		var item AuditEvent
		var detailsRaw []byte
		var createdAt time.Time
		if err = rows.Scan(&item.ID, &item.ActorUserID, &item.Action, &detailsRaw, &createdAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
			return
		}
		_ = json.Unmarshal(detailsRaw, &item.Details)
		item.CreatedAt = toISO(createdAt)
		audit = append(audit, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kpi": map[string]any{
			"sales":   round2(sales),
			"payouts": round2(payouts),
			"margin":  round2(sales - payouts),
			"draws":   drawsCount,
			"tickets": ticketsCount,
		},
		"recentAudit": audit,
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.clientsMu.Lock()
	s.clients[conn] = struct{}{}
	s.clientsMu.Unlock()

	_ = conn.WriteJSON(map[string]any{"type": "system", "payload": "Connected to draw stream"})
	for {
		if _, _, err = conn.ReadMessage(); err != nil {
			s.clientsMu.Lock()
			delete(s.clients, conn)
			s.clientsMu.Unlock()
			_ = conn.Close()
			return
		}
	}
}

func (s *Server) drawActionRouter(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "draws" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
		return
	}
	drawID := parts[2]

	if len(parts) == 5 && parts[3] == "tickets" && parts[4] == "buy" && r.Method == http.MethodPost {
		s.handleBuyTicket(w, r, drawID)
		return
	}
	if len(parts) == 5 && parts[3] == "admin" && r.Method == http.MethodPost {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Manual draw control is disabled. Draws start and run only by scheduler."})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
}

func method(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return
		}
		h(w, r)
	}
}

func migrate(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  name TEXT NOT NULL,
  role TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS wallet_transactions (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id),
  type TEXT NOT NULL,
	amount NUMERIC(10,2) NOT NULL,
	balance_after NUMERIC(10,2) NOT NULL,
  meta JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_wallet_transactions_user_created ON wallet_transactions(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS draws (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  draw_at TIMESTAMPTZ NOT NULL,
  ticket_price NUMERIC(14,2) NOT NULL,
  max_number INT NOT NULL,
  numbers_count INT NOT NULL,
  status TEXT NOT NULL,
  winning_numbers INT[] NOT NULL DEFAULT ARRAY[]::INT[],
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS tickets (
  id UUID PRIMARY KEY,
	serial_number BIGINT UNIQUE,
  user_id UUID NOT NULL REFERENCES users(id),
  draw_id UUID NOT NULL REFERENCES draws(id),
  numbers INT[] NOT NULL,
  status TEXT NOT NULL,
  win_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE(user_id, draw_id, numbers)
);
CREATE INDEX IF NOT EXISTS idx_tickets_user_created ON tickets(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS notifications (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id),
  type TEXT NOT NULL,
  message TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS audit_events (
  id UUID PRIMARY KEY,
  actor_user_id UUID NOT NULL REFERENCES users(id),
  action TEXT NOT NULL,
  details JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	if _, err = db.Exec(`ALTER TABLE wallet_transactions ALTER COLUMN amount TYPE NUMERIC(10,2) USING ROUND(amount::numeric, 2)`); err != nil {
		return err
	}
	if _, err = db.Exec(`ALTER TABLE wallet_transactions ALTER COLUMN balance_after TYPE NUMERIC(10,2) USING ROUND(balance_after::numeric, 2)`); err != nil {
		return err
	}

	var exists int
	err = db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = 'admin@loto.local'`).Scan(&exists)
	if err != nil {
		return err
	}
	if exists == 0 {
		hash, _ := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		adminID := uuid.NewString()
		if _, err = db.Exec(`
			INSERT INTO users (id, email, password_hash, name, role)
			VALUES ($1, 'admin@loto.local', $2, 'Administrator', 'admin')
		`, adminID, string(hash)); err != nil {
			return err
		}
		if _, err = db.Exec(`
			INSERT INTO wallet_transactions (id, user_id, type, amount, balance_after, meta)
			VALUES ($1, $2, 'init', 0, 0, '{}'::jsonb)
		`, uuid.NewString(), adminID); err != nil {
			return err
		}
	}

	if _, err = db.Exec(`CREATE SEQUENCE IF NOT EXISTS tickets_serial_number_seq`); err != nil {
		return err
	}
	if _, err = db.Exec(`ALTER TABLE tickets ADD COLUMN IF NOT EXISTS serial_number BIGINT`); err != nil {
		return err
	}
	if _, err = db.Exec(`UPDATE tickets SET serial_number = nextval('tickets_serial_number_seq') WHERE serial_number IS NULL`); err != nil {
		return err
	}
	if _, err = db.Exec(`
		SELECT setval(
			'tickets_serial_number_seq',
			GREATEST(COALESCE((SELECT MAX(serial_number) FROM tickets), 0), 1),
			COALESCE((SELECT MAX(serial_number) FROM tickets), 0) > 0
		)
	`); err != nil {
		return err
	}
	if _, err = db.Exec(`ALTER TABLE tickets ALTER COLUMN serial_number SET DEFAULT nextval('tickets_serial_number_seq')`); err != nil {
		return err
	}
	if _, err = db.Exec(`ALTER TABLE tickets ALTER COLUMN serial_number SET NOT NULL`); err != nil {
		return err
	}

	return nil
}

func waitDB(db *sql.DB) error {
	deadline := time.Now().Add(40 * time.Second)
	for {
		if err := db.Ping(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("database is unavailable")
		}
		time.Sleep(1200 * time.Millisecond)
	}
}

func nextSlotTime(now time.Time, interval time.Duration) time.Time {
	nowUTC := now.UTC()
	truncated := nowUTC.Truncate(interval)
	if truncated.Equal(nowUTC) {
		return nowUTC.Add(interval)
	}
	return truncated.Add(interval)
}

func ensureStandardDraws(db *sql.DB) error {
	interval := time.Duration(standardDrawIntervalMin) * time.Minute
	next := nextSlotTime(time.Now(), interval)

	for i := 0; i < standardDrawFutureCount; i++ {
		slot := next.Add(time.Duration(i) * interval)
		name := fmt.Sprintf("%s %s", standardDrawBaseName, slot.Format("15:04"))

		var exists int
		err := db.QueryRow(`
			SELECT COUNT(*)
			FROM draws
			WHERE draw_at = $1 AND name = $2
		`, slot, name).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			continue
		}

		_, err = db.Exec(`
			INSERT INTO draws (id, name, draw_at, ticket_price, max_number, numbers_count, status, winning_numbers)
			VALUES ($1, $2, $3, 50, $4, $5, 'scheduled', $6)
		`, uuid.NewString(), name, slot, lottoBarrelsCount, lottoDrawnNumbersCount, pq.Array([]int{}))
		if err != nil {
			return err
		}
	}

	return nil
}

func normalizeActiveDrawRules(db *sql.DB) error {
	_, err := db.Exec(`
		UPDATE draws
		SET max_number = $1,
		    numbers_count = $2
		WHERE status IN ('scheduled', 'running')
	`, lottoBarrelsCount, lottoDrawnNumbersCount)
	return err
}

func normalizeFinishedDrawWinningNumbers(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT id, winning_numbers
		FROM draws
		WHERE status = 'finished'
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type drawFix struct {
		id             string
		winningNumbers []int
	}
	toFix := make([]drawFix, 0)

	for rows.Next() {
		var id string
		var winningRaw []int64
		if err = rows.Scan(&id, pq.Array(&winningRaw)); err != nil {
			return err
		}
		winning := int64SliceToInt(winningRaw)
		if len(winning) == lottoDrawnNumbersCount {
			continue
		}
		used := make(map[int]bool, lottoBarrelsCount)
		for _, n := range winning {
			if n >= 1 && n <= lottoBarrelsCount {
				used[n] = true
			}
		}
		remaining := make([]int, 0, lottoBarrelsCount)
		for n := 1; n <= lottoBarrelsCount; n++ {
			if !used[n] {
				remaining = append(remaining, n)
			}
		}
		rand.Shuffle(len(remaining), func(i, j int) {
			remaining[i], remaining[j] = remaining[j], remaining[i]
		})
		if len(winning) > lottoDrawnNumbersCount {
			winning = winning[:lottoDrawnNumbersCount]
		} else {
			need := lottoDrawnNumbersCount - len(winning)
			if need > len(remaining) {
				need = len(remaining)
			}
			winning = append(winning, remaining[:need]...)
		}
		toFix = append(toFix, drawFix{id: id, winningNumbers: winning})
	}

	for _, item := range toFix {
		if _, err = db.Exec(`
			UPDATE draws
			SET winning_numbers = $2
			WHERE id = $1
		`, item.id, pq.Array(item.winningNumbers)); err != nil {
			return err
		}
	}

	return nil
}

func normalizeFinishedTicketResults(db *sql.DB) error {
	_, err := db.Exec(`
		WITH calc AS (
			SELECT
				t.id,
				(
					SELECT COUNT(*)
					FROM unnest(t.numbers) AS n
					WHERE n = ANY(d.winning_numbers)
				) AS matches
			FROM tickets t
			JOIN draws d ON d.id = t.draw_id
			WHERE d.status = 'finished'
			  AND cardinality(d.winning_numbers) >= $1
		)
		UPDATE tickets t
		SET status = CASE
				WHEN calc.matches >= $2 THEN 'won'
				ELSE 'lost'
			END,
			win_amount = CASE
				WHEN calc.matches = $3 THEN $4
				WHEN calc.matches = $5 THEN $6
				WHEN calc.matches = $7 THEN $8
				ELSE 0
			END
		FROM calc
		WHERE t.id = calc.id
	`, lottoDrawnNumbersCount, lottoTicketNumbersCount-2, lottoTicketNumbersCount, prizeBig, lottoTicketNumbersCount-1, prizeMedium, lottoTicketNumbersCount-2, prizeSmall)
	return err
}

func normalizeWalletBalances(db *sql.DB) error {
	_, err := db.Exec(`
		WITH ordered AS (
			SELECT
				ctid,
				ROUND(SUM(amount) OVER (
					PARTITION BY user_id
					ORDER BY created_at, ctid
					ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
				), 2)::numeric AS computed_balance
			FROM wallet_transactions
		)
		UPDATE wallet_transactions w
		SET balance_after = ordered.computed_balance
		FROM ordered
		WHERE w.ctid = ordered.ctid
		  AND w.balance_after <> ordered.computed_balance
	`)
	return err
}

func applyRetentionPolicies(db *sql.DB) error {
	if notificationsRetentionDays > 0 {
		result, err := db.Exec(`
			DELETE FROM notifications
			WHERE created_at < NOW() - ($1::int * INTERVAL '1 day')
		`, notificationsRetentionDays)
		if err != nil {
			return err
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
			log.Printf("retention: deleted %d notifications older than %d days", rows, notificationsRetentionDays)
		}
	}

	if auditRetentionDays > 0 {
		result, err := db.Exec(`
			DELETE FROM audit_events
			WHERE created_at < NOW() - ($1::int * INTERVAL '1 day')
		`, auditRetentionDays)
		if err != nil {
			return err
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
			log.Printf("retention: deleted %d audit events older than %d days", rows, auditRetentionDays)
		}
	}

	return nil
}

func (s *Server) tryRunAutoDraws() {
	select {
	case s.autoDrawSem <- struct{}{}:
		defer func() { <-s.autoDrawSem }()
	default:
		return
	}

	rows, err := s.db.Query(`
		SELECT id
		FROM draws
		WHERE status IN ('scheduled', 'running')
		  AND draw_at <= NOW()
		ORDER BY draw_at ASC
		LIMIT 10
	`)
	if err != nil {
		log.Printf("auto draw query error: %v", err)
		return
	}
	defer rows.Close()

	drawIDs := make([]string, 0, 10)
	for rows.Next() {
		var drawID string
		if scanErr := rows.Scan(&drawID); scanErr != nil {
			log.Printf("auto draw scan error: %v", scanErr)
			return
		}
		drawIDs = append(drawIDs, drawID)
	}

	for _, drawID := range drawIDs {
		if err = s.executeDrawAutomatically(drawID); err != nil {
			log.Printf("auto draw %s error: %v", drawID, err)
		}
	}
}

func (s *Server) executeDrawAutomatically(drawID string) error {
	ctx := context.Background()

	for {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}

		var draw Draw
		var winningNumbersRaw []int64
		var drawAt, createdAt time.Time
		var startedAt, finishedAt sql.NullTime
		err = tx.QueryRowContext(ctx, `
			SELECT id, name, draw_at, ticket_price, max_number, numbers_count, status, winning_numbers, created_at, started_at, finished_at
			FROM draws
			WHERE id = $1
			FOR UPDATE
		`, drawID).Scan(&draw.ID, &draw.Name, &drawAt, &draw.TicketPrice, &draw.MaxNumber, &draw.NumbersCount, &draw.Status, pq.Array(&winningNumbersRaw), &createdAt, &startedAt, &finishedAt)
		if err != nil {
			tx.Rollback()
			return err
		}
		draw.WinningNumbers = int64SliceToInt(winningNumbersRaw)

		if draw.Status == "finished" {
			tx.Rollback()
			return nil
		}

		startedNow := false
		if draw.Status == "scheduled" {
			if _, err = tx.ExecContext(ctx, `
				UPDATE draws
				SET status = 'running', started_at = NOW()
				WHERE id = $1
			`, drawID); err != nil {
				tx.Rollback()
				return err
			}
			draw.Status = "running"
			draw.StartedAt = nowISO()
			startedNow = true
		}

		if draw.Status != "running" {
			tx.Rollback()
			return nil
		}

		if len(draw.WinningNumbers) < lottoDrawnNumbersCount {
			pool := make([]int, 0)
			used := map[int]bool{}
			for _, n := range draw.WinningNumbers {
				used[n] = true
			}
			for i := 1; i <= lottoBarrelsCount; i++ {
				if !used[i] {
					pool = append(pool, i)
				}
			}
			nextNumber := pool[rand.Intn(len(pool))]
			draw.WinningNumbers = append(draw.WinningNumbers, nextNumber)

			if _, err = tx.ExecContext(ctx, `
				UPDATE draws
				SET winning_numbers = $2
				WHERE id = $1
			`, drawID, pq.Array(draw.WinningNumbers)); err != nil {
				tx.Rollback()
				return err
			}

			if err = tx.Commit(); err != nil {
				return err
			}

			if startedNow {
				s.broadcast(map[string]any{"type": "draw.started", "payload": draw})
			}
			s.broadcast(map[string]any{"type": "draw.number", "payload": map[string]any{"drawId": drawID, "number": nextNumber, "winningNumbers": draw.WinningNumbers}})
			time.Sleep(700 * time.Millisecond)
			continue
		}

		type settledTicket struct {
			ticketID string
			userID   string
			numbers  []int
		}

		rows, qErr := tx.QueryContext(ctx, `
			SELECT id, user_id, numbers
			FROM tickets
			WHERE draw_id = $1
			FOR UPDATE
		`, drawID)
		if qErr != nil {
			tx.Rollback()
			return qErr
		}

		toSettle := make([]settledTicket, 0)
		for rows.Next() {
			var ticketID, userID string
			var numbersRaw []int64
			if err = rows.Scan(&ticketID, &userID, pq.Array(&numbersRaw)); err != nil {
				rows.Close()
				tx.Rollback()
				return err
			}

			numbers := make([]int, len(numbersRaw))
			for i, number := range numbersRaw {
				numbers[i] = int(number)
			}
			toSettle = append(toSettle, settledTicket{ticketID: ticketID, userID: userID, numbers: numbers})
		}
		rows.Close()

		for _, ticket := range toSettle {
			matches := 0
			for _, n := range ticket.numbers {
				for _, wn := range draw.WinningNumbers {
					if n == wn {
						matches++
					}
				}
			}

			win := calculateWin(matches)
			status := "lost"
			if win > 0 {
				status = "won"
			}
			if _, err = tx.ExecContext(ctx, `
				UPDATE tickets
				SET status = $2, win_amount = $3
				WHERE id = $1
			`, ticket.ticketID, status, win); err != nil {
				tx.Rollback()
				return err
			}

			if win > 0 {
				if _, err = s.addTransactionTx(ctx, tx, ticket.userID, "winning_credit", win, map[string]any{"drawId": drawID, "ticketId": ticket.ticketID}); err != nil {
					tx.Rollback()
					return err
				}
				if err = s.notifyTx(ctx, tx, ticket.userID, "winning", fmt.Sprintf("Выигрыш %.2f по тиражу %s", win, draw.Name)); err != nil {
					tx.Rollback()
					return err
				}
			}
		}

		if _, err = tx.ExecContext(ctx, `
			UPDATE draws
			SET status = 'finished', finished_at = NOW(), winning_numbers = $2
			WHERE id = $1
		`, drawID, pq.Array(draw.WinningNumbers)); err != nil {
			tx.Rollback()
			return err
		}

		draw.Status = "finished"
		draw.FinishedAt = nowISO()

		if err = tx.Commit(); err != nil {
			return err
		}

		if startedNow {
			s.broadcast(map[string]any{"type": "draw.started", "payload": draw})
		}
		s.broadcast(map[string]any{"type": "draw.finished", "payload": draw})
		return nil
	}
}

func startStandardDrawScheduler(s *Server) {
	if err := normalizeWalletBalances(s.db); err != nil {
		log.Printf("wallet balance normalize error: %v", err)
	}
	if err := normalizeActiveDrawRules(s.db); err != nil {
		log.Printf("draw rules normalize error: %v", err)
	}
	if err := normalizeFinishedDrawWinningNumbers(s.db); err != nil {
		log.Printf("draw winning normalize error: %v", err)
	}
	if err := normalizeFinishedTicketResults(s.db); err != nil {
		log.Printf("ticket result normalize error: %v", err)
	}
	if err := ensureStandardDraws(s.db); err != nil {
		log.Printf("standard draw init error: %v", err)
	}
	if err := applyRetentionPolicies(s.db); err != nil {
		log.Printf("retention init error: %v", err)
	}
	s.tryRunAutoDraws()

	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if err := normalizeActiveDrawRules(s.db); err != nil {
				log.Printf("draw rules normalize scheduler error: %v", err)
			}
			if err := normalizeFinishedDrawWinningNumbers(s.db); err != nil {
				log.Printf("draw winning normalize scheduler error: %v", err)
			}
			if err := normalizeFinishedTicketResults(s.db); err != nil {
				log.Printf("ticket result normalize scheduler error: %v", err)
			}
			if err := ensureStandardDraws(s.db); err != nil {
				log.Printf("standard draw scheduler error: %v", err)
			}
		}
	}()

	go func() {
		interval := time.Duration(retentionJobIntervalMin) * time.Minute
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			if err := applyRetentionPolicies(s.db); err != nil {
				log.Printf("retention scheduler error: %v", err)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			s.tryRunAutoDraws()
		}
	}()
}

func main() {
	rand.Seed(time.Now().UnixNano())
	port := os.Getenv("PORT")
	if port == "" {
		port = "4000"
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "dev-secret-change-me"
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://postgres:postgres@db:5432/loto?sslmode=disable"
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err = waitDB(db); err != nil {
		log.Fatal(err)
	}
	if err = migrate(db); err != nil {
		log.Fatal(err)
	}

	srv := newServer(db, secret)
	startStandardDrawScheduler(srv)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", srv.withCORS(method(http.MethodGet, srv.handleHealth)))
	mux.HandleFunc("/api/metrics", srv.withCORS(method(http.MethodGet, srv.handleMetrics)))
	mux.HandleFunc("/api/auth/register", srv.withCORS(method(http.MethodPost, srv.handleRegister)))
	mux.HandleFunc("/api/auth/login", srv.withCORS(method(http.MethodPost, srv.handleLogin)))
	mux.HandleFunc("/api/me", srv.withCORS(method(http.MethodGet, srv.handleMe)))
	mux.HandleFunc("/api/wallet", srv.withCORS(method(http.MethodGet, srv.handleWallet)))
	mux.HandleFunc("/api/wallet/deposit", srv.withCORS(method(http.MethodPost, srv.handleDeposit)))
	mux.HandleFunc("/api/wallet/withdraw", srv.withCORS(method(http.MethodPost, srv.handleWithdraw)))
	mux.HandleFunc("/api/draws", srv.withCORS(method(http.MethodGet, srv.handleDraws)))
	mux.HandleFunc("/api/draws/admin/create", srv.withCORS(method(http.MethodPost, srv.handleCreateDraw)))
	mux.HandleFunc("/api/draws/", srv.withCORS(srv.drawActionRouter))
	mux.HandleFunc("/api/my/tickets", srv.withCORS(method(http.MethodGet, srv.handleMyTickets)))
	mux.HandleFunc("/api/notifications", srv.withCORS(method(http.MethodGet, srv.handleNotifications)))
	mux.HandleFunc("/api/admin/reports", srv.withCORS(method(http.MethodGet, srv.handleAdminReports)))
	mux.HandleFunc("/ws", srv.handleWS)
	mux.HandleFunc("/", srv.withCORS(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
	}))

	log.Printf("Go backend listening on :%s", port)
	log.Printf("Admin account: admin@loto.local / admin123")
	log.Fatal(http.ListenAndServe(":"+port, srv.withMetrics(mux)))
}
