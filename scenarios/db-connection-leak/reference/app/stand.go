// Package stand implements the shared stand code used by every task
// environment. It is NOT editable by the participant: only app/handler.go
// (plus optional app/migrations/*.sql) is the task-specific, editable surface.
//
// The stand provides /healthz, /metrics, /debug/reset, honest Prometheus
// counters, a background worker queue with a persistent jobs table, and
// /debug/invariant/<name>. It is compiled into the service binary by the
// Builder and deployed by Docker Compose.
//
// A task scenario syncs this package via scripts/sync-stand.sh: it copies
// internal/stand/*.go into app/ (as package main), generates the thin
// app/main.go that calls Serve, and leaves exactly the editable files
// (app/handler.go and app/migrations/*.sql) to the task author.
package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is everything the stand service takes from its environment, with the
// mounted config file (/etc/app/app.conf) layered on top (Part B2).
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

// ConfigFilePath is where the compose engine mounts the task configuration
// (the workspace config/ directory, B2). The stand reads it as an overlay on
// the environment: a config-level task is fixed by editing this file, so the
// service must load what the participant actually wrote.
const ConfigFilePath = "/etc/app/app.conf"

func loadConfig() Config {
	cfg := Config{
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
	// The mounted config file wins over the environment: it is what the
	// participant edits, so it is what the service must obey.
	for key, value := range readConfigOverlay(ConfigFilePath) {
		applyConfigValue(&cfg, key, value)
	}
	return cfg
}

// applyConfigValue maps one config-file key onto the typed Config field.
func applyConfigValue(cfg *Config, key, value string) {
	value = strings.TrimSpace(value)
	switch key {
	case "PORT":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.Port = parsed
		}
	case "POOL_MAX":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.PoolMax = parsed
		}
	case "DB_HOST":
		cfg.DBHost = value
	case "DB_PORT":
		cfg.DBPort = value
	case "DB_NAME":
		cfg.DBName = value
	case "DB_USER":
		cfg.DBUser = value
	case "DB_PASSWORD":
		cfg.DBPass = value
	case "DB_APP_NAME":
		cfg.DBAppName = value
	case "LOG_LEVEL":
		cfg.LogLevel = value
	case "LOG_FILE":
		cfg.LogFile = value
	}
}

// readConfigOverlay parses the mounted key=value configuration file. A missing
// file is not an error: the service then runs on its environment alone.
func readConfigOverlay(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return out
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
	mu sync.Mutex
	// DB is the live database pool used by the task service. Keeping the
	// reference in the shared stand lets /metrics expose database/sql's real
	// pool gauges instead of a synthetic counter.
	DB             *sql.DB
	startedAt      time.Time
	requests       int64
	errors         int64
	dbQueriesTotal int64
	depCalls       int64
	cacheEntries   int64
	cacheHits      int64
	retries        int64
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

func (s *State) incRequests()  { s.mu.Lock(); s.requests++; s.mu.Unlock() }
func (s *State) incErrors()    { s.mu.Lock(); s.errors++; s.mu.Unlock() }
func (s *State) incDBQueries() { s.mu.Lock(); s.dbQueriesTotal++; s.mu.Unlock() }
func (s *State) incDepCalls()  { s.mu.Lock(); s.depCalls++; s.mu.Unlock() }

// SetCacheEntries publishes the current number of live cache entries. A cache
// without a bound grows forever, and this gauge is how the quality gate sees
// that: the stand never invents the number, the task handler owns it.
func (s *State) SetCacheEntries(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheEntries = n
}

// AddCacheHits records requests answered from the cache. A synthetic counter
// (for example "hits = requests") would make a penetration or stampede check
// meaningless, so the stand only exposes what the handler actually observed.
func (s *State) AddCacheHits(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheHits += n
}

// AddRetries records retried attempts. The retry storm incident is graded on
// this counter: retries must exist but stay bounded.
func (s *State) AddRetries(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries += n
}

// SetDiskUsagePct publishes the measured disk usage of the task volume. The
// number must come from the filesystem (statfs), not from a simulation.
func (s *State) SetDiskUsagePct(pct float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diskPct = pct
}

// AddLogBytes records bytes actually written to the log sink.
func (s *State) AddLogBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logVolumeB += n
}

// SetDBLatencyP95 publishes the p95 latency the handler measured itself.
func (s *State) SetDBLatencyP95(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbLatencyP95 = d
}

// SetDBTableStats publishes real table statistics (dead tuples, live rows)
// read from pg_stat_user_tables.
func (s *State) SetDBTableStats(deadTuples, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbDeadTuples = deadTuples
	s.dbRows = rows
}

// SetDBReachable updates the database reachability observed by the probe.
func (s *State) SetDBReachable(ok bool, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbReachable = ok
	if ok {
		s.dbLatencyP95 = latency
	}
}

func (s *State) snapshot() (requests, errors, dbQueriesTotal, depCalls, cacheEntries, cacheHits, retries int64, diskPct float64, logVolumeB int64, dbReachable bool, dbLatencyP95 time.Duration, dbDeadTuples, dbRows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.errors, s.dbQueriesTotal, s.depCalls, s.cacheEntries, s.cacheHits, s.retries, s.diskPct, s.logVolumeB, s.dbReachable, s.dbLatencyP95, s.dbDeadTuples, s.dbRows
}

// MetricsHandler returns a Prometheus-text metrics handler for the stand.
func MetricsHandler(state *State, logFile string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requests, errors, dbQueriesTotal, depCalls, cacheEntries, cacheHits, retries, diskPct, logVolumeB, dbReachable, dbLatencyP95, dbDeadTuples, dbRows := state.snapshot()
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
		poolInUse, poolMax := 0, 0
		if state.DB != nil {
			stats := state.DB.Stats()
			poolInUse = stats.InUse
			poolMax = stats.MaxOpenConnections
		}
		fmt.Fprintf(w, "# HELP app_db_pool_in_use database connections currently in use\n# TYPE app_db_pool_in_use gauge\napp_db_pool_in_use %d\n", poolInUse)
		fmt.Fprintf(w, "# HELP app_db_pool_max database connection pool limit\n# TYPE app_db_pool_max gauge\napp_db_pool_max %d\n", poolMax)
		fmt.Fprintf(w, "# HELP app_dep_calls_total dependency calls issued\n# TYPE app_dep_calls_total counter\napp_dep_calls_total %d\n", depCalls)
		fmt.Fprintf(w, "# HELP app_cache_entries entries currently cached\n# TYPE app_cache_entries gauge\napp_cache_entries %d\n", cacheEntries)
		fmt.Fprintf(w, "# HELP app_disk_pct measured disk usage of the task volume\n# TYPE app_disk_pct gauge\napp_disk_pct %.1f\n", diskPct)
		fmt.Fprintf(w, "# HELP app_log_bytes bytes written by the logger\n# TYPE app_log_bytes counter\napp_log_bytes %d\n", logVolumeB)
		fmt.Fprintf(w, "# HELP app_db_reachable whether the database answered\n# TYPE app_db_reachable gauge\napp_db_reachable %d\n", boolToInt(dbReachable))
		fmt.Fprintf(w, "# HELP app_db_query_latency_p95_ms measured order-query latency\n# TYPE app_db_query_latency_p95_ms gauge\napp_db_query_latency_p95_ms %d\n", dbLatencyP95.Milliseconds())
		fmt.Fprintf(w, "# HELP app_db_dead_tuples dead tuples the table statistics report\n# TYPE app_db_dead_tuples gauge\napp_db_dead_tuples %d\n", dbDeadTuples)
		fmt.Fprintf(w, "# HELP app_db_rows rows in the orders table\n# TYPE app_db_rows gauge\napp_db_rows %d\n", dbRows)
		fmt.Fprintf(w, "# HELP app_retry_requests requests answered after retries\n# TYPE app_retry_requests counter\napp_retry_requests %d\n", retries)
		fmt.Fprintf(w, "# HELP app_cache_hits_total requests answered from cache\n# TYPE app_cache_hits_total counter\napp_cache_hits_total %d\n", cacheHits)
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
