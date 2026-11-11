package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

// db is the shared database connection, used by handler.go.
var db *sql.DB

func main() {
	cfg := loadConfig()
	state := newState()

	var err error
	db, err = sql.Open("postgres", fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPass, cfg.DBName))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Run migrations from app/migrations/*.sql.
	if entries, err := os.ReadDir("app/migrations"); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
				data, err := os.ReadFile("app/migrations/" + entry.Name())
				if err != nil {
					log.Fatalf("read migration %s: %v", entry.Name(), err)
				}
				for _, stmt := range splitSQLStatements(string(data)) {
					if _, err := db.Exec(stmt); err != nil {
						log.Fatalf("migration %s: %v", entry.Name(), err)
					}
				}
			}
		}
	}

	// Seed sample data (idempotent).
	if _, err := db.Exec(`INSERT INTO orders (customer, total) SELECT 'customer-' || g, g * 10 FROM generate_series(1, 1000) g ON CONFLICT DO NOTHING`); err != nil {
		log.Printf("seed orders: %v (may already be seeded)", err)
	}

	// Probe the database periodically so health/metrics are current.
	go probeDatabase(db, state)

	mux := http.NewServeMux()
	mux.Handle("/healthz", HealthHandler(state))
	mux.Handle("/metrics", MetricsHandler(state, cfg.LogFile))
	mux.Handle("/debug/reset", ResetHandler(state))

	// Task-specific routes from handler.go.
	if handlerRoutes != nil {
		handlerRoutes(mux)
	} else {
		mux.HandleFunc("/api/orders", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unimplemented handler"}`, http.StatusInternalServerError)
		})
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, `{"status":"ok"}`)
		})
	}

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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func probeDatabase(db *sql.DB, state *State) {
	for {
		start := time.Now()
		if err := db.Ping(); err != nil {
			state.incErrors()
			time.Sleep(2 * time.Second)
			continue
		}
		latency := time.Since(start)
		state.incDBQueries()
		state.SetDBReachable(true, latency)
		time.Sleep(5 * time.Second)
	}
}

func splitSQLStatements(content string) []string {
	var out []string
	for _, chunk := range strings.Split(content, ";") {
		var lines []string
		for _, line := range strings.Split(chunk, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			lines = append(lines, line)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
