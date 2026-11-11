package main

import (
	"fmt"
	"net/http"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", ordersHandler)
}

// ordersHandler lists orders. A watcher goroutine is started for the request
// and waits on a channel the request never signals: the goroutine stays
// blocked after the response has been written.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	finished := make(chan struct{})
	go func() {
		<-finished
	}()
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
