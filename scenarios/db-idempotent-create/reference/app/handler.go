package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// ensureIdempotentOnce guards the one-time schema bootstrap below.
var ensureIdempotentOnce sync.Once

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	// FIXED: one row per client key is a property of the stored data, so the
	// rows written while every POST was accepted are collapsed and a unique
	// index keeps the key unique from now on.
	ensureIdempotentOnce.Do(func() {
		db.Exec(`DELETE FROM orders WHERE id NOT IN (SELECT min(id) FROM orders GROUP BY client_key)`)
		db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS orders_client_key_key ON orders (client_key)`)
	})
	mux.HandleFunc("/api/orders", ordersHandler)
	mux.HandleFunc("/debug/invariant/idempotency", InvariantHandler(orderStorageInvariant))
}

// orderRequest is the payload of POST /api/orders.
type orderRequest struct {
	ClientKey string  `json:"client_key"`
	Customer  string  `json:"customer"`
	Total     float64 `json:"total"`
}

// ordersHandler stores an order (POST) or lists them (GET). FIXED: a repeated
// POST of an already stored client_key returns the stored row.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM orders`).Scan(&count); err != nil {
			state.incErrors()
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		state.incDBQueries()
		state.SetQueriesPerRequest(1)
		fmt.Fprintf(w, `{"orders_count":%d}`, count)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req orderRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			state.incErrors()
			http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
			return
		}
	}
	if req.ClientKey == "" {
		req.ClientKey = "anonymous"
	}
	stored, err := db.Exec(`INSERT INTO orders (client_key, customer, total) VALUES ($1, $2, $3) ON CONFLICT (client_key) DO NOTHING`,
		req.ClientKey, req.Customer, req.Total)
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"insert failed"}`, http.StatusInternalServerError)
		return
	}
	added, _ := stored.RowsAffected()
	state.incDBQueries()
	state.SetQueriesPerRequest(1)
	fmt.Fprintf(w, `{"client_key":%q,"created":%v}`, req.ClientKey, added > 0)
}

// orderStorageInvariant reports whether the stored data itself keeps a client
// key to a single row, which is what makes a create idempotent.
func orderStorageInvariant(name string) (bool, string) {
	var uniqueKeys int
	err := db.QueryRow(`
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'orders' AND indexdef LIKE '%UNIQUE%' AND indexdef LIKE '%(client_key%'`).Scan(&uniqueKeys)
	if err != nil {
		return false, err.Error()
	}
	if uniqueKeys == 0 {
		return false, "orders.client_key is not unique: a repeated POST stores another row"
	}
	return true, "orders.client_key is unique"
}
