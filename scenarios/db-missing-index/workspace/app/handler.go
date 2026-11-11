package main

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", ordersHandler)
}

// latencies keeps the last 50 measured query durations. The p95 of that
// window is what the handler publishes as app_db_query_latency_p95_ms, so a
// plan that reads the whole table shows up as a number instead of a one-off
// spike that averages away.
type latencies struct {
	mu      sync.Mutex
	samples []time.Duration
}

// observe records one measurement and returns the p95 of the window.
func (l *latencies) observe(sample time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.samples) == 50 {
		l.samples = l.samples[1:]
	}
	l.samples = append(l.samples, sample)
	if len(l.samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), l.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[(len(sorted)-1)*95/100]
}

// window is the latency window the handler reports.
var window = &latencies{}

// ordersHandler lists the orders of one customer.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.Atoi(r.URL.Query().Get("user_id"))
	if err != nil || userID <= 0 {
		http.Error(w, `{"error":"user_id must be a positive integer"}`, http.StatusBadRequest)
		return
	}
	state.incRequests()

	start := time.Now()
	rows, err := db.Query("SELECT id, total FROM orders WHERE user_id = $1", userID)
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	var count int
	for rows.Next() {
		var id int
		var total float64
		if err := rows.Scan(&id, &total); err != nil {
			rows.Close()
			state.incErrors()
			http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
			return
		}
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		state.incErrors()
		http.Error(w, `{"error":"rows failed"}`, http.StatusInternalServerError)
		return
	}
	rows.Close()
	state.incDBQueries()

	elapsed := time.Since(start)
	p95 := window.observe(elapsed)
	state.SetDBLatencyP95(p95)
	log.Printf("orders of user %d: rows=%d query_ms=%d p95_ms=%d", userID, count, elapsed.Milliseconds(), p95.Milliseconds())

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"user_id":%d,"rows":%d,"query_ms":%d,"p95_ms":%d}`,
		userID, count, elapsed.Milliseconds(), p95.Milliseconds())
}
