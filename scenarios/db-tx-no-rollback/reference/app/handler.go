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

// ordersHandler lists the orders of the store.
//
// A request with ?flaky=1 models a client that abandons the request in the
// middle of the work: the handler answers 500 and returns at once, without
// finishing the unit of work it already opened.
//
// FIXED: the unit of work is closed on every exit path, so a connection is
// never left holding an open transaction. defer tx.Rollback() right after
// db.Begin() covers the early exit as well as the error paths; the successful
// path commits and the deferred call returns ErrTxDone, which is a no-op.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()

	tx, err := db.Begin()
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"begin failed"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	rows, err := tx.Query("SELECT id, total FROM orders")
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}

	var ids []int
	for rows.Next() {
		var id int
		var total float64
		if err := rows.Scan(&id, &total); err != nil {
			state.incErrors()
			http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		state.incErrors()
		http.Error(w, `{"error":"rows failed"}`, http.StatusInternalServerError)
		return
	}
	rows.Close()
	state.incDBQueries()

	if r.URL.Query().Get("flaky") == "1" {
		// The client is gone: answer 500 and leave. The deferred rollback
		// still returns the connection to the pool.
		state.incErrors()
		http.Error(w, `{"error":"flaky client"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		state.incErrors()
		http.Error(w, `{"error":"commit failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"orders_count":%d}`, len(ids))
}
