// Command incidentsim-depstub is the controllable dependency of the `dep`
// task stack (Part E). It is a tiny Go service that behaves like a flaky
// downstream: an operator (or the replay hooks of a verification) steers it
// through POST /control, and the task service under test talks to it over
// ordinary HTTP.
//
// Control fields:
//
//	error_rate  fraction of requests answered with 500 (0..1)
//	latency_ms  artificial delay added to every request
//	down        when true every request fails fast with 503
//	hang_pct    fraction of requests that stall for hang_ms
//
// The stub reports honest Prometheus metrics so the quality gate can grade
// the *caller's* behaviour (dep_calls, retries) against the stub's own
// observed outcomes.
package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// control is the mutable state of the stub, guarded by a mutex because the
// replay hooks and the request handlers run concurrently.
type control struct {
	mu         sync.Mutex
	errorRate  float64
	latencyMS  int
	down       bool
	hangPct    float64
	hangMS     int
	requests   atomic.Int64
	errors     atomic.Int64
	depCalls   atomic.Int64
	configured atomic.Value // float64 snapshot for /metrics
}

func (c *control) snapshot() (errorRate float64, latencyMS int, down bool, hangPct float64, hangMS int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errorRate, c.latencyMS, c.down, c.hangPct, c.hangMS
}

func (c *control) apply(body map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := body["error_rate"].(float64); ok {
		c.errorRate = v
	}
	if v, ok := body["latency_ms"].(float64); ok {
		c.latencyMS = int(v)
	}
	if v, ok := body["down"].(bool); ok {
		c.down = v
	}
	if v, ok := body["body"].(string); ok && v != "" {
		// A raw JSON body is accepted as-is for callers that post the hook
		// body verbatim.
		var parsed map[string]any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			c.apply(parsed)
		}
	}
	if v, ok := body["hang_pct"].(float64); ok {
		c.hangPct = v
	}
	if v, ok := body["hang_ms"].(float64); ok {
		c.hangMS = int(v)
	}
	c.configured.Store(c.errorRate)
}

func main() {
	port := envInt("PORT", 9090)
	c := &control{}
	c.configured.Store(float64(0))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		c.apply(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"configured"}`))
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		errorRate, latencyMS, down, hangPct, hangMS := c.snapshot()
		if down {
			c.errors.Add(1)
			http.Error(w, `{"error":"dependency down"}`, http.StatusServiceUnavailable)
			return
		}
		if hangPct > 0 && rand.Float64() < hangPct {
			if hangMS <= 0 {
				hangMS = 5000
			}
			time.Sleep(time.Duration(hangMS) * time.Millisecond)
		}
		if latencyMS > 0 {
			time.Sleep(time.Duration(latencyMS) * time.Millisecond)
		}
		if errorRate > 0 && rand.Float64() < errorRate {
			c.errors.Add(1)
			http.Error(w, `{"error":"dependency failure"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","source":"depstub"}`))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		errorRate, _, down, _, _ := c.snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetric(w, "dep_requests_total", float64(c.requests.Load()))
		writeMetric(w, "dep_errors_total", float64(c.errors.Load()))
		writeMetric(w, "dep_down", boolFloat(down))
		writeMetric(w, "dep_error_rate", errorRate)
	})

	addr := ":" + strconv.Itoa(port)
	log.Printf("depstub listening on %s", addr)
	if err := (&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe(); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func writeMetric(w http.ResponseWriter, name string, value float64) {
	_, _ = w.Write([]byte(name + " " + strconv.FormatFloat(value, 'f', -1, 64) + "\n"))
}

func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}
