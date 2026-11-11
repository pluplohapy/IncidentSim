// Package stand implements the shared stand code used by every task
// environment. It is NOT editable by the participant: only app/handler.go
// (plus optional app/migrations/*.sql) is the task-specific, editable surface.
//
// The stand provides /healthz, /metrics, /debug/reset, honest Prometheus
// counters, a background worker queue with a persistent jobs table, and
// /debug/invariant/<name>. It is compiled into the service binary by the
// Builder and deployed by the engine (compose or Firecracker).
//
// A task scenario syncs this package via scripts/sync-stand.sh: it copies
// internal/stand/*.go into app/ and leaves exactly one editable file
// (app/handler.go) plus the optional migrations.
package stand

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Config is everything the stand service takes from its environment.
// Values come from env; a task may also mount a config file (Part B2), but
// the stand itself reads env only — the task's handler reads the config file.
type Config struct {
	Port      int
	DBHost    string
	DBPort    string
	DBName    string
	DBUser    string
	DBPass    string
	DBAppName string
	PoolMax   int
	LogLevel  string
	LogFile   string
}

func loadConfig() Config {
	return Config{
		Port:      envInt("PORT", 8080),
		DBHost:    env("DB_HOST", "127.0.0.1"),
		DBPort:    env("DB_PORT", "5432"),
		DBName:    env("DB_NAME", "orders"),
		DBUser:    env("DB_USER", "postgres"),
		DBPass:    env("DB_PASSWORD", "postgres"),
		DBAppName: env("DB_APP_NAME", "app"),
		PoolMax:   envInt("POOL_MAX", 20),
		LogLevel:  env("LOG_LEVEL", "info"),
		LogFile:   env("LOG_FILE", ""),
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

// State holds the mutable service state: counters and gauges that the metrics
// endpoint reports honestly (db.Stats(), runtime.NumGoroutine,
// /proc/self/fd, uptime).
type State struct {
	mu             sync.Mutex
	startedAt      time.Time
	requests       int64
	errors         int64
	dbQueriesTotal int64
	depCalls       int64
	cacheEntries   int64
	diskPct        float64
	logVolumeB     int64
	dbReachable    bool
	dbLatencyP95   time.Duration
	dbDeadTuples   int
	dbRows         int
}

func newState() *State {
	return &State{startedAt: time.Now().UTC()}
}

func (s *State) incRequests() { s.mu.Lock(); s.requests++; s.mu.Unlock() }
func (s *State) incErrors()   { s.mu.Lock(); s.errors++; s.mu.Unlock() }
func (s *State) incDBQueries() { s.mu.Lock(); s.dbQueriesTotal++; s.mu.Unlock() }
func (s *State) incDepCalls()  { s.mu.Lock(); s.depCalls++; s.mu.Unlock() }

// SetDBReachable updates the database reachability observed by the probe.
func (s *State) SetDBReachable(ok bool, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbReachable = ok
	if ok {
		s.dbLatencyP95 = latency
	}
}

func (s *State) snapshot() (requests, errors, dbQueriesTotal, depCalls, cacheEntries int64, diskPct float64, logVolumeB int64, dbReachable bool, dbLatencyP95 time.Duration, dbDeadTuples, dbRows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.errors, s.dbQueriesTotal, s.depCalls, s.cacheEntries, s.diskPct, s.logVolumeB, s.dbReachable, s.dbLatencyP95, s.dbDeadTuples, s.dbRows
}

// MetricsHandler returns a Prometheus-text metrics handler for the stand.
func MetricsHandler(state *State, logFile string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requests, errors, dbQueriesTotal, depCalls, cacheEntries, diskPct, logVolumeB, dbReachable, dbLatencyP95, dbDeadTuples, dbRows := state.snapshot()
		var goroutines int64 = int64(runtime.NumGoroutine())
		var openFDs int64
		if f, err := os.Open("/proc/self/fd"); err == nil {
			if names, err := f.Readdirnames(0); err == nil {
				openFDs = int64(len(names))
			}
			f.Close()
		}
		uptime := time.Since(state.startedAt).Seconds()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP app_requests_total requests served\n# TYPE app_requests_total counter\napp_requests_total %d\n", requests)
		fmt.Fprintf(w, "# HELP app_errors_total requests that failed\n# TYPE app_errors_total counter\napp_errors_total %d\n", errors)
		fmt.Fprintf(w, "# HELP app_db_queries_total database queries issued\n# TYPE app_db_queries_total counter\napp_db_queries_total %d\n", dbQueriesTotal)
		fmt.Fprintf(w, "# HELP app_dep_calls_total dependency calls issued\n# TYPE app_dep_calls_total counter\napp_dep_calls_total %d\n", depCalls)
		fmt.Fprintf(w, "# HELP app_cache_entries entries currently cached\n# TYPE app_cache_entries gauge\napp_cache_entries %d\n", cacheEntries)
		fmt.Fprintf(w, "# HELP app_disk_pct simulated disk usage\n# TYPE app_disk_pct gauge\napp_disk_pct %.1f\n", diskPct)
		fmt.Fprintf(w, "# HELP app_log_bytes bytes written by the logger\n# TYPE app_log_bytes counter\napp_log_bytes %d\n", logVolumeB)
		fmt.Fprintf(w, "# HELP app_db_reachable whether the database answered\n# TYPE app_db_reachable gauge\napp_db_reachable %d\n", boolToInt(dbReachable))
		fmt.Fprintf(w, "# HELP app_db_query_latency_p95_ms measured order-query latency\n# TYPE app_db_query_latency_p95_ms gauge\napp_db_query_latency_p95_ms %d\n", dbLatencyP95.Milliseconds())
		fmt.Fprintf(w, "# HELP app_db_dead_tuples dead tuples the table statistics report\n# TYPE app_db_dead_tuples gauge\napp_db_dead_tuples %d\n", dbDeadTuples)
		fmt.Fprintf(w, "# HELP app_db_rows rows in the orders table\n# TYPE app_db_rows gauge\napp_db_rows %d\n", dbRows)
		fmt.Fprintf(w, "# HELP app_retry_requests requests answered after retries\n# TYPE app_retry_requests counter\napp_retry_requests %d\n", requests)
		fmt.Fprintf(w, "# HELP app_cache_hits_total requests answered from cache\n# TYPE app_cache_hits_total counter\napp_cache_hits_total %d\n", requests)
		fmt.Fprintf(w, "# HELP go_goroutines number of goroutines\n# TYPE go_goroutines gauge\ngo_goroutines %d\n", goroutines)
		fmt.Fprintf(w, "# HELP go_heap_bytes heap memory in use\n# TYPE go_heap_bytes gauge\n")
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fmt.Fprintf(w, "go_heap_bytes %d\n", m.HeapInuse)
		fmt.Fprintf(w, "# HELP open_fds open file descriptors\n# TYPE open_fds gauge\nopen_fds %d\n", openFDs)
		fmt.Fprintf(w, "# HELP uptime_seconds service uptime\n# TYPE uptime_seconds gauge\nuptime_seconds %.0f\n", uptime)
	}
}

// boolToInt converts bool to int (1 for true, 0 for false).
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// ResetHandler clears the error and request counters (used by the verifier
// before an observation window).
func ResetHandler(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.errors = 0
		state.requests = 0
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"counters reset"}`)
	}
}

// HealthHandler reports whether the service is healthy.
func HealthHandler(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		reachable := state.dbReachable
		state.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if !reachable {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"status":"degraded","reason":"db unreachable"}`)
			return
		}
		fmt.Fprintln(w, `{"status":"ok"}`)
	}
}

// Run starts the stand HTTP server and blocks until a signal is received.
func Run(cfg Config, state *State, handler http.Handler) error {
	mux := http.NewServeMux()
	mux.Handle("/healthz", HealthHandler(state))
	mux.Handle("/metrics", MetricsHandler(state, cfg.LogFile))
	mux.Handle("/debug/reset", ResetHandler(state))
	mux.Handle("/", handler)

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
	return server.Shutdown(ctx)
}
