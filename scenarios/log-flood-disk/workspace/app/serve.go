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

	// The driver registers itself with database/sql: without it every task
	// service dies at boot with "unknown driver postgres" and no endpoint ever
	// answers, which surfaces as a Start timeout instead of a usable session.
	_ "github.com/lib/pq"
)

// db is the database pool the task handler uses. The stand assigns it before
// the task routes are registered, so a handler closure always observes a live
// pool. It lives here (and not in the generated main.go) so both the task
// workspace and the reference package share one declaration.
var db *sql.DB

// state holds the honest service counters the stand publishes on /metrics.
var state = newState()

// OnShutdown is the task's shutdown hook. The stand default drains in-flight
// requests within a bounded grace period, which is what a production service
// does on SIGTERM. A task whose incident is "SIGTERM kills the process
// immediately" overrides this hook in its handler, and that override is the
// defect the participant has to find and remove.
//
// The hook exists because the shutdown behaviour must be observable through
// the single editable file, not through a second hidden file.
var OnShutdown func(ctx context.Context) error

// Serve runs one task service of the stand: database pool, migrations from
// app/migrations/*.sql, the shared debug endpoints, the task routes and a
// graceful shutdown. It blocks until a termination signal arrives.
//
// Every generated workspace calls Serve from its generated main.go; the
// participant never edits this path.
func Serve(cfg Config, routes func(*http.ServeMux)) {
	pool, err := sql.Open("postgres", fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable application_name=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPass, cfg.DBName, cfg.DBAppName))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db = pool
	state.DB = db
	db.SetMaxOpenConns(cfg.PoolMax)
	db.SetMaxIdleConns(cfg.PoolMax)
	db.SetConnMaxLifetime(time.Hour)

	RunMigrations(db)
	go ProbeDatabase(db, state)

	mux := http.NewServeMux()
	mux.Handle("/healthz", HealthHandler(state))
	mux.Handle("/metrics", MetricsHandler(state, cfg.LogFile))
	mux.Handle("/debug/reset", ResetHandler(state))
	if routes != nil {
		routes(mux)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unimplemented handler"}`, http.StatusInternalServerError)
		})
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("stand listening on :%d", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if OnShutdown != nil {
		// A hook that exits the process or returns without draining models
		// the ungraceful-shutdown incident; the default path drains.
		if err := OnShutdown(ctx); err != nil {
			log.Printf("shutdown hook: %v", err)
		}
		return
	}
	_ = server.Shutdown(ctx)
}

// RunMigrations applies app/migrations/*.sql in lexical order. Migrations are
// part of the task surface: a missing index is created by a migration file.
//
// The database is a sibling container that may still be initialising, so the
// set is retried until it succeeds or the deadline passes: a service that
// exits on the first connection refusal never comes up.
func RunMigrations(db *sql.DB) {
	entries, err := os.ReadDir("app/migrations")
	if err != nil {
		return
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		lastErr := applyMigrations(db, entries)
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			log.Fatalf("%v", lastErr)
		}
		log.Printf("migration pending: %v (retrying)", lastErr)
		time.Sleep(2 * time.Second)
	}
}

// applyMigrations runs every statement once and stops at the first error.
func applyMigrations(db *sql.DB, entries []os.DirEntry) error {
	var applied int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := os.ReadFile("app/migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		for _, stmt := range splitSQLStatements(string(data)) {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("migration %s: %w", entry.Name(), err)
			}
			applied++
		}
	}
	if applied > 0 {
		log.Printf("migrations applied: %d statements", applied)
	}
	return nil
}

// splitSQLStatements splits a migration file into statements on semicolons.
//
// Comment lines are dropped first, and only then the text is split: a
// semicolon inside a -- comment would otherwise cut the statement in half and
// the database would be asked to execute comment prose as SQL.
func splitSQLStatements(content string) []string {
	var code strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	var out []string
	for _, stmt := range strings.Split(code.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// ProbeDatabase keeps health and the metrics honest about database state, and
// measures the real latency of a trivial statement.
func ProbeDatabase(db *sql.DB, state *State) {
	for {
		start := time.Now()
		if err := db.Ping(); err != nil {
			state.incErrors()
			state.SetDBReachable(false, 0)
			time.Sleep(2 * time.Second)
			continue
		}
		latency := time.Since(start)
		state.incDBQueries()
		state.SetDBReachable(true, latency)
		state.SetDBLatencyP95(latency)
		observeTableStats(db, state)
		time.Sleep(5 * time.Second)
	}
}

// observeTableStats reads live/dead tuple counts of the task tables straight
// from pg_stat_user_tables, so a bloat metric is the database's own answer.
func observeTableStats(db *sql.DB, state *State) {
	var deadTuples, rows int
	err := db.QueryRow(`
		SELECT COALESCE(SUM(n_dead_tup),0), COALESCE(SUM(n_live_tup),0)
		FROM pg_stat_user_tables`).Scan(&deadTuples, &rows)
	if err != nil {
		return
	}
	state.SetDBTableStats(deadTuples, rows)
}
