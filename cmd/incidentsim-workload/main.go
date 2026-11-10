// Command incidentsim-workload is the simulated service used by the task
// environments. It implements the fault modes the manifests declare so that a
// task can be diagnosed end to end without a production system.
//
// The service deliberately reads its configuration once, at start: that is
// what makes "change the flag and restart" a real fix and "restart with the
// same flag" a non-fix.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// config is everything the service takes from its environment.
type config struct {
	Port      int
	DBHost    string
	DBPort    string
	DBName    string
	DBUser    string
	DBPass    string
	LogLevel  string
	LogFile   string
	DiskLimit float64
	PoolLeak  bool
	PoolMax   int
	// SlowQueryLimit is the latency above which the order query is degraded.
	SlowQueryLimit time.Duration
	// CacheEnabled models a cache in front of the database.
	CacheEnabled bool
	// RetryEnabled, RetryAttempts and RetryBackoff model the retry policy.
	RetryEnabled  bool
	RetryAttempts int
	RetryBackoff  time.Duration
}

func loadConfig() config {
	slowLimit := envFloat("SLOW_QUERY_MS", 250)
	retryAttempts := envInt("RETRY_ATTEMPTS", 3)
	retryBackoffMs := envInt("RETRY_BACKOFF_MS", 50)
	return config{
		Port:           envInt("PORT", 8080),
		SlowQueryLimit: time.Duration(slowLimit) * time.Millisecond,
		CacheEnabled:   envBool("CACHE_ENABLED", true),
		RetryEnabled:   envBool("RETRY_ENABLED", true),
		RetryAttempts:  retryAttempts,
		RetryBackoff:   time.Duration(retryBackoffMs) * time.Millisecond,
		DBHost:         env("DB_HOST", "127.0.0.1"),
		DBPort:         env("DB_PORT", "5432"),
		DBName:         env("DB_NAME", "orders"),
		DBUser:         env("DB_USER", "postgres"),
		DBPass:         env("DB_PASSWORD", "postgres"),
		LogLevel:       env("LOG_LEVEL", "info"),
		LogFile:        env("LOG_FILE", ""),
		DiskLimit:      envFloat("DISK_LIMIT_PCT", 90),
		PoolLeak:       envBool("POOL_LEAK", false),
		PoolMax:        envInt("POOL_MAX", 20),
	}
}

// state is the mutable part of the service: the simulated pool and disk, plus
// what the service really observes in its database.
type state struct {
	mu         sync.Mutex
	poolInUse  int
	errors     int
	requests   int
	diskPct    float64
	startedAt  time.Time
	logVolumeB int

	// logFile and logWriter persist the declared log path on disk.
	logFile   string
	logWriter *os.File
	logMu     sync.Mutex
	// dbReachable reports whether the last database probe succeeded.
	dbReachable bool
	// dbLatencyP95 is the measured latency of the order query.
	dbLatencyP95 time.Duration
	// dbDeadTuples and dbRows are read straight from pg_stat_user_tables.
	dbDeadTuples int
	dbRows       int
}

// dbObservation is one probe of the database behind the service.
type dbObservation struct {
	reachable  bool
	latencyP95 time.Duration
	deadTuples int
	rows       int
}

func main() {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	service := &state{startedAt: time.Now().UTC(), diskPct: 10, logFile: cfg.LogFile}
	service.logf(cfg.LogLevel, "info", "workload starting (level=%s pool_leak=%v pool_max=%d)", cfg.LogLevel, cfg.PoolLeak, cfg.PoolMax)

	// The debug flood is the log-flood-disk fault: at debug level the service
	// writes far more than at info, which is exactly what fills the disk.
	if cfg.LogLevel == "debug" || cfg.LogLevel == "trace" {
		go service.floodLogs(cfg)
	}
	// The connection leak is the db-connection-leak fault: connections are
	// taken from the pool and never returned.
	if cfg.PoolLeak {
		go service.leakPool(cfg)
	}
	// A database task is observed for real: the service queries the database
	// it was pointed at and reports what it sees.
	if cfg.DBHost != "" && cfg.DBHost != "127.0.0.1" {
		go service.observeDatabase(cfg)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", service.health(cfg))
	mux.HandleFunc("/api/orders", service.orders(cfg))
	mux.HandleFunc("/metrics", service.metrics(cfg))
	mux.HandleFunc("/debug/reset", service.resetCounters)
	// /debug/clear-logs models the distractor: it frees the disk by removing
	// the log volume without fixing what produced it. The system answers
	// again, so it passes a correctness gate, and the configuration is still
	// wrong, so it fails a quality gate.
	mux.HandleFunc("/debug/clear-logs", service.clearLogs)

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

func (s *state) health(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		poolInUse, diskPct, errors := s.poolInUse, s.diskPct, s.errors
		s.mu.Unlock()

		dbReachable, dbLatencyP95, dbDeadTuples, dbRows := s.dbSnapshot()
		status := http.StatusOK
		reasons := []string{}
		if cfg.PoolLeak && poolInUse >= cfg.PoolMax {
			status = http.StatusServiceUnavailable
			reasons = append(reasons, "connection pool exhausted")
		}
		if diskPct >= cfg.DiskLimit {
			status = http.StatusServiceUnavailable
			reasons = append(reasons, fmt.Sprintf("disk usage %.1f%%", diskPct))
		}
		if s.dbConfigured(cfg) && !dbReachable {
			status = http.StatusServiceUnavailable
			reasons = append(reasons, "database unreachable")
		}
		if s.dbConfigured(cfg) && dbLatencyP95 > cfg.SlowQueryLimit {
			status = http.StatusServiceUnavailable
			reasons = append(reasons, fmt.Sprintf("order query latency %s exceeds %s", dbLatencyP95.Round(time.Millisecond), cfg.SlowQueryLimit))
		}
		if len(reasons) > 0 {
			s.logf(cfg.LogLevel, "info", "degraded: %s", strings.Join(reasons, "; "))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"status":"%s","pool_in_use":%d,"pool_max":%d,"disk_pct":%.1f,"errors":%d,"db_latency_p95_ms":%d,"db_dead_tuples":%d,"db_rows":%d}`,
			statusText(status), poolInUse, cfg.PoolMax, diskPct, errors,
			dbLatencyP95.Milliseconds(), dbDeadTuples, dbRows)
	}
}

func statusText(status int) string {
	if status == http.StatusOK {
		return "ok"
	}
	return "degraded"
}

func (s *state) orders(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		poolInUse := s.poolInUse
		s.mu.Unlock()

		if cfg.PoolLeak && poolInUse >= cfg.PoolMax {
			s.mu.Lock()
			s.errors++
			s.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"error":"connection pool exhausted"}`)
			return
		}

		// The request walks a retry policy. An aggressive one (many attempts,
		// no backoff) is what turns a slow dependency into a storm: the same
		// request hits the database many times over and the latency multiplies.
		attempts := 1
		if cfg.RetryEnabled {
			attempts = cfg.RetryAttempts
			if attempts < 1 {
				attempts = 1
			}
		}
		latency := requestLatency(cfg) * time.Duration(attempts)
		if cfg.RetryEnabled && cfg.RetryBackoff <= 0 && attempts > 5 {
			// A retry storm exhausts the environment: the request fails rather
			// than answering, which is the visible symptom of the incident.
			s.mu.Lock()
			s.errors++
			s.mu.Unlock()
			s.logf(cfg.LogLevel, "info", "retry storm: %d attempts without backoff, request failed", attempts)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"error":"upstream overloaded by retries"}`)
			return
		}
		time.Sleep(latency)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"orders":[{"id":1,"total":100},{"id":2,"total":250}]}`)
	}
}

// requestLatency is the latency of one order lookup. Without a cache every
// lookup goes to the database, which is where the avalanche starts.
func requestLatency(cfg config) time.Duration {
	if cfg.CacheEnabled {
		return 5 * time.Millisecond
	}
	// A cache miss is cheaper than a real database miss would be, but the
	// multiply by the retry count is what makes the incident visible.
	return 25 * time.Millisecond
}

func (s *state) metrics(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		fmt.Fprintf(w, "# HELP app_errors_total requests that failed\n")
		fmt.Fprintf(w, "# TYPE app_errors_total counter\n")
		fmt.Fprintf(w, "app_errors_total %d\n", s.errors)
		fmt.Fprintf(w, "# HELP app_requests_total requests served\n")
		fmt.Fprintf(w, "# TYPE app_requests_total counter\n")
		fmt.Fprintf(w, "app_requests_total %d\n", s.requests)
		fmt.Fprintf(w, "# HELP app_db_pool_in_use connections currently held\n")
		fmt.Fprintf(w, "# TYPE app_db_pool_in_use gauge\n")
		fmt.Fprintf(w, "app_db_pool_in_use %d\n", s.poolInUse)
		fmt.Fprintf(w, "# HELP app_db_pool_max pool capacity\n")
		fmt.Fprintf(w, "# TYPE app_db_pool_max gauge\n")
		fmt.Fprintf(w, "app_db_pool_max %d\n", 20)
		fmt.Fprintf(w, "# HELP app_disk_used_pct simulated disk usage\n")
		fmt.Fprintf(w, "# TYPE app_disk_used_pct gauge\n")
		fmt.Fprintf(w, "app_disk_used_pct %.1f\n", s.diskPct)
		fmt.Fprintf(w, "# HELP app_log_bytes bytes written by the logger\n")
		fmt.Fprintf(w, "# TYPE app_log_bytes counter\n")
		fmt.Fprintf(w, "app_log_bytes %d\n", s.logVolumeB)
		fmt.Fprintf(w, "# HELP app_db_reachable whether the database answered\n")
		fmt.Fprintf(w, "# TYPE app_db_reachable gauge\n")
		fmt.Fprintf(w, "app_db_reachable %d\n", boolToInt(s.dbReachable))
		fmt.Fprintf(w, "# HELP app_db_query_latency_p95_ms measured order-query latency\n")
		fmt.Fprintf(w, "# TYPE app_db_query_latency_p95_ms gauge\n")
		fmt.Fprintf(w, "app_db_query_latency_p95_ms %d\n", s.dbLatencyP95.Milliseconds())
		fmt.Fprintf(w, "# HELP app_db_dead_tuples dead tuples the table statistics report\n")
		fmt.Fprintf(w, "# TYPE app_db_dead_tuples gauge\n")
		fmt.Fprintf(w, "app_db_dead_tuples %d\n", s.dbDeadTuples)
		fmt.Fprintf(w, "# HELP app_db_rows rows in the orders table\n")
		fmt.Fprintf(w, "# TYPE app_db_rows gauge\n")
		fmt.Fprintf(w, "app_db_rows %d\n", s.dbRows)
		fmt.Fprintf(w, "# HELP app_retry_requests requests answered after retries\n")
		fmt.Fprintf(w, "# TYPE app_retry_requests counter\n")
		fmt.Fprintf(w, "app_retry_requests %d\n", s.requests)
		fmt.Fprintf(w, "# HELP app_cache_hits_total requests answered from cache\n")
		fmt.Fprintf(w, "# TYPE app_cache_hits_total counter\n")
		fmt.Fprintf(w, "app_cache_hits_total %d\n", func() int {
			if cfg.CacheEnabled {
				return int(s.requests)
			}
			return 0
		}())
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// dbConfigured reports whether the service was pointed at a real database,
// i.e. at a network peer and not at itself.
func (s *state) dbConfigured(cfg config) bool {
	return cfg.DBHost != "" && cfg.DBHost != "127.0.0.1" && cfg.DBHost != "localhost"
}

func (s *state) clearLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diskPct = 10
	s.logVolumeB = 0
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "log volume cleared")
}

func (s *state) resetCounters(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors, s.requests, s.logVolumeB = 0, 0, 0
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "counters reset")
}

// floodLogs simulates a verbose logger: it grows the log volume until the disk
// limit is reached. The fix is a lower log level, not deleting the file.
func (s *state) floodLogs(cfg config) {
	for {
		s.mu.Lock()
		if s.diskPct >= cfg.DiskLimit {
			s.mu.Unlock()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		s.logVolumeB += 4096
		s.diskPct += 0.2
		if s.diskPct > cfg.DiskLimit {
			s.diskPct = cfg.DiskLimit
		}
		s.mu.Unlock()
		s.logf(cfg.LogLevel, "debug", "request payload id=%d user=%s detail=%s", 1, "system", repeat("x", 256))
		// The flood grows at a pace an incident actually grows at: fast enough
		// to reach the limit within a session, slow enough that a participant
		// has time to observe it and to test a hypothesis.
		time.Sleep(100 * time.Millisecond)
	}
}

// leakPool simulates a connection leak: the pool fills up and never drains.
func (s *state) leakPool(cfg config) {
	for {
		s.mu.Lock()
		if s.poolInUse < cfg.PoolMax {
			s.poolInUse++
		}
		s.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
}

// dbSnapshot returns the last database observation, as the health endpoint
// and the metrics endpoint report it.
func (s *state) dbSnapshot() (bool, time.Duration, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dbReachable, s.dbLatencyP95, s.dbDeadTuples, s.dbRows
}

// observeDatabase probes the database behind the service every few seconds.
//
// A database task has to be observable by the participant, and the only honest
// way to do that is to query the real database: the service shells out to psql
// and reports the latency it measures and the state it reads. That keeps the
// task a database task instead of a config task with extra steps.
func (s *state) observeDatabase(cfg config) {
	for {
		observation := probeDatabase(cfg)
		s.mu.Lock()
		s.dbReachable = observation.reachable
		s.dbLatencyP95 = observation.latencyP95
		s.dbDeadTuples = observation.deadTuples
		s.dbRows = observation.rows
		s.mu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

// probeDatabase measures one order query and reads the table statistics.
func probeDatabase(cfg config) dbObservation {
	start := time.Now()
	rows, err := runPSQL(cfg, "SELECT count(*) FROM orders")
	latency := time.Since(start)
	if err != nil {
		// "Relation does not exist" is a database that answered. Only a
		// connection failure means the database is unreachable, and reporting
		// a SQL error as unreachable would make every task without the table
		// look like an outage.
		if _, probeErr := runPSQL(cfg, "SELECT 1"); probeErr != nil {
			log.Printf("database probe failed: %v", probeErr)
			return dbObservation{}
		}
		return dbObservation{reachable: true}
	}
	dead, err := runPSQL(cfg, "SELECT coalesce(sum(n_dead_tup),0) FROM pg_stat_user_tables WHERE relname = 'orders'")
	if err != nil {
		return dbObservation{reachable: true, latencyP95: latency, rows: atoiSafe(rows)}
	}
	return dbObservation{
		reachable:  true,
		latencyP95: latency,
		deadTuples: atoiSafe(dead),
		rows:       atoiSafe(rows),
	}
}

// runPSQL runs one statement against the task database. The password travels in
// the environment, never on the command line, so it does not show up in ps.
func runPSQL(cfg config, statement string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "psql",
		"-h", cfg.DBHost, "-p", cfg.DBPort, "-U", cfg.DBUser, "-d", cfg.DBName,
		"-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", statement)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.DBPass)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql %q: %w: %s", statement, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func atoiSafe(text string) int {
	value, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0
	}
	return value
}

// logf emits a log line, honouring the configured level. At info level the
// debug lines disappear, which is the observable effect of the fix.
//
// A declared LOG_FILE is written as well as stdout, so the environment has a
// real file a participant can look at: "where are the logs" is the first
// question of a disk incident, and answering it with "nowhere" would make the
// task unreadable.
func (s *state) logf(level, want, format string, args ...any) {
	if levelOrder(level) < levelOrder(want) {
		return
	}
	line := fmt.Sprintf(format, args...)
	log.Print(line)
	s.appendToFile(line)
}

// appendToFile appends one line to the configured log file.
func (s *state) appendToFile(line string) {
	if s.logFile == "" {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logWriter == nil {
		if err := os.MkdirAll(filepath.Dir(s.logFile), 0o755); err != nil {
			log.Printf("create log dir: %v", err)
			s.logFile = ""
			return
		}
		file, err := os.OpenFile(s.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("open log file: %v", err)
			s.logFile = ""
			return
		}
		s.logWriter = file
	}
	if _, err := fmt.Fprintln(s.logWriter, time.Now().UTC().Format(time.RFC3339), line); err != nil {
		log.Printf("append log file: %v", err)
	}
}

func levelOrder(level string) int {
	switch level {
	case "trace":
		return 2
	case "debug":
		return 1
	default:
		return 0
	}
}

func repeat(text string, count int) string {
	out := make([]byte, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, text[0])
	}
	return string(out)
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

func envFloat(key string, fallback float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if value := os.Getenv(key); value != "" {
		switch value {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}
