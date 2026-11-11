package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

// config is everything the service takes from its environment.
type config struct {
	Port      int
	DBHost    string
	DBPort    string
	DBName    string
	DBUser    string
	DBPass    string
	DBAppName string
	PoolMax   int
}

func loadConfig() config {
	return config{
		Port:      envInt("PORT", 8080),
		DBHost:    env("DB_HOST", "127.0.0.1"),
		DBPort:    env("DB_PORT", "5432"),
		DBName:    env("DB_NAME", "orders"),
		DBUser:    env("DB_USER", "postgres"),
		DBPass:    env("DB_PASSWORD", "postgres"),
		DBAppName: env("DB_APP_NAME", "app"),
		PoolMax:   envInt("POOL_MAX", 20),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

// state holds the mutable service state.
type state struct {
	mu       sync.Mutex
	errors   int
	requests int
	db       *sql.DB
}

var serviceState = &state{}

func (s *state) metrics(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		fmt.Fprintf(w, "# HELP app_db_pool_in_use connections currently held\n")
		fmt.Fprintf(w, "# TYPE app_db_pool_in_use gauge\n")
		fmt.Fprintf(w, "app_db_pool_in_use %d\n", s.db.Stats().InUse)
		fmt.Fprintf(w, "# HELP app_db_pool_max pool capacity\n")
		fmt.Fprintf(w, "# TYPE app_db_pool_max gauge\n")
		// The gauge reports the limit the pool was actually opened with, not a
		// constant: a diagnosis that compares in_use against a wrong max
		// blames the wrong artefact.
		fmt.Fprintf(w, "app_db_pool_max %d\n", cfg.PoolMax)
		fmt.Fprintf(w, "# HELP app_errors_total requests that failed\n")
		fmt.Fprintf(w, "# TYPE app_errors_total counter\n")
		fmt.Fprintf(w, "app_errors_total %d\n", s.errors)
		fmt.Fprintf(w, "# HELP app_requests_total requests served\n")
		fmt.Fprintf(w, "# TYPE app_requests_total counter\n")
		fmt.Fprintf(w, "app_requests_total %d\n", s.requests)
	}
}

// fail records a failed request and answers 500.
func (s *state) fail(w http.ResponseWriter) {
	s.mu.Lock()
	s.errors++
	s.mu.Unlock()
	http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
}

// BROKEN: the transaction is never closed and its rows are never closed, so
// every request checks a connection out of the pool for good. The backend
// holds it in "idle in transaction" until the pool runs dry.
func (s *state) orders() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()

		// BUG: транзакция и rows не закрываются — соединение навсегда остаётся
		// в пуле. db.Begin() открывает транзакцию без контекста, поэтому
		// никто не откатывает её при выходе из обработчика, а tx.Query
		// оставляет результат непрочитанным: соединение остаётся выданным
		// и держит бэкенд в состоянии idle in transaction.
		//
		// Запрос намеренно идёт без дедлайна запроса: отмена контекста на
		// выходе из обработчика закрыла бы соединение на стороне сервера, и
		// утечка превратилась бы в самоисцеление вместо инцидента.
		tx, err := s.db.Begin()
		if err != nil {
			s.fail(w)
			return
		}
		rows, err := tx.Query("SELECT id, total FROM orders")
		if err != nil {
			s.fail(w)
			return
		}

		var id int
		var total float64
		if rows.Next() {
			_ = rows.Scan(&id, &total)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"orders":[{"id":1,"total":100},{"id":2,"total":250}]}`)
	}
}

func (s *state) healthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"status":"degraded"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	}
}

func waitForSignal(server *http.Server) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func main() {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s application_name=%s sslmode=disable",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPass, cfg.DBName, cfg.DBAppName)
	var err error
	serviceState.db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer serviceState.db.Close()

	serviceState.db.SetMaxOpenConns(cfg.PoolMax)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", serviceState.healthz())
	mux.HandleFunc("/api/orders", serviceState.orders())
	mux.HandleFunc("/metrics", serviceState.metrics(cfg))

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("listening on :%d", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	waitForSignal(server)
}
