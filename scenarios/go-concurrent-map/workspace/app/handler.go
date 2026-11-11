package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", ordersHandler)
	mux.HandleFunc("/api/count", countHandler)
}

// counters holds how often each key has been counted. Eight goroutines of one
// request update it at the same time.
var counters = map[string]int{}

// requestMu serialises a whole request; CONCURRENCY_MODE=global-mutex turns it on.
var requestMu sync.Mutex

// ordersHandler lists orders.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM orders`).Scan(&count); err != nil {
		state.incErrors()
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	state.incDBQueries()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"orders_count":%d}`, count)
}

// countHandler counts the requested key from eight goroutines at once and
// answers with the total.
func countHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	key := r.URL.Query().Get("k")
	if key == "" {
		key = "0"
	}
	globalMode := strings.EqualFold(os.Getenv("CONCURRENCY_MODE"), "global-mutex")
	if globalMode {
		requestMu.Lock()
		defer requestMu.Unlock()
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if globalMode {
				time.Sleep(800 * time.Millisecond)
			}
			for i := 0; i < 50; i++ {
				counters[key] = counters[key] + 1
			}
		}()
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"key":%q,"count":%d}`, key, counters[key])
}
