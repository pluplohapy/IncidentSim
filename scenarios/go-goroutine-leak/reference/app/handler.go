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

// ordersHandler lists orders. FIXED: the watcher goroutine ends with the
// request it was started for, because it waits on the request context instead
// of a channel that is never signalled.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	go func() {
		<-r.Context().Done()
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
