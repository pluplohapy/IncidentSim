package main

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/dep", depHandler)
	mux.HandleFunc("/api/orders", ordersHandler)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}

// depBaseURL is the base URL of the downstream dependency.
func depBaseURL() string {
	return "http://" + envOr("DEP_HOST", "depstub") + ":" + envOr("DEP_PORT", "9090")
}

func depTimeout() time.Duration {
	ms := envIntOr("DEP_TIMEOUT_MS", 5000)
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// retryBackoff grows 50ms*2^(attempt-1) and adds jitter below the base delay.
func retryBackoff(attempt int) time.Duration {
	step := time.Duration(50<<uint(attempt-1)) * time.Millisecond
	return step + time.Duration(rand.Int63n(int64(step)))
}

func writeDepOK(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func drainClose(resp *http.Response) {
	if resp != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}
}

// depHandler calls the dependency with a bounded policy: at most three
// attempts, an exponential backoff with jitter between them, and a fast
// fallback answer when the dependency stays unavailable.
func depHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	retriesEnabled := envBoolOr("RETRY_ENABLED", true)
	maxAttempts := envIntOr("RETRY_MAX", 3)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	client := &http.Client{Timeout: depTimeout()}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		state.incDepCalls()
		resp, err := client.Get(depBaseURL() + "/work")
		if err == nil && resp.StatusCode < 500 {
			writeDepOK(w, resp)
			return
		}
		drainClose(resp)
		if !retriesEnabled || attempt >= maxAttempts {
			break
		}
		backoff := retryBackoff(attempt)
		log.Printf("bounded retry: dependency attempt=%d failed, sleeping %s", attempt, backoff)
		state.AddRetries(1)
		time.Sleep(backoff)
	}
	log.Printf("bounded retry: dependency calls exhausted, serving fallback")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"source":"fallback"}`))
}

// ordersHandler lists orders from the database, closing the transaction and
// rows so the connection returns to the pool.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, `{"error":"begin failed"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	rows, err := tx.Query("SELECT id, total FROM orders")
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		var total float64
		if err := rows.Scan(&id, &total); err != nil {
			http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, `{"error":"rows error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"orders_count":%d}`, len(ids))
}
