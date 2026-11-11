package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", ordersHandler)
	mux.Handle("/debug/invariant/freshness", InvariantHandler(freshnessInvariant))
}

// orderRow is one order of the list the API serves, with the positions that
// belong to it.
type orderRow struct {
	id       int
	customer string
	total    float64
	skus     []string
}

// ordersLimit is the page size of the list endpoint. Zero means "all rows".
func ordersLimit() int {
	if raw := os.Getenv("ORDERS_LIMIT"); raw != "" {
		if limit, err := strconv.Atoi(raw); err == nil && limit > 0 {
			return limit
		}
	}
	return 0
}

// limitClause renders the optional LIMIT of the list query.
func limitClause(limit int) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf(" LIMIT %d", limit)
}

// queryStats keeps the number of database round trips of the last 50 served
// requests. queries/requests is the number an N+1 inflates, and folding it
// into a moving average is what makes it a gauge instead of a burst that
// averages away behind unrelated traffic.
type queryStats struct {
	mu      sync.Mutex
	samples []int
	total   int
}

// observe records one request and returns the current average.
func (q *queryStats) observe(count int) float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.samples) == 50 {
		q.total -= q.samples[0]
		q.samples = q.samples[1:]
	}
	q.samples = append(q.samples, count)
	q.total += count
	return float64(q.total) / float64(len(q.samples))
}

// queries is the average the stand publishes as app_db_queries_per_request.
var queries = &queryStats{}

// createOrder writes one order and returns its id.
func createOrder(customer string) (int, error) {
	var id int
	err := db.QueryRow("INSERT INTO orders (customer, total) VALUES ($1, $2) RETURNING id",
		customer, len(customer)).Scan(&id)
	return id, err
}

// freshnessInvariant is the read-your-write check of the list endpoint: an
// order written through this process must be visible in the list the API
// serves, and the list must hold every row the database reports. A view that
// is served from anywhere but the database cannot satisfy it.
func freshnessInvariant(name string) (bool, string) {
	id, err := createOrder("freshness-probe")
	if err != nil {
		return false, fmt.Sprintf("write order: %v", err)
	}
	rows, _, err := listOrders()
	if err != nil {
		return false, fmt.Sprintf("list orders: %v", err)
	}
	var stored int
	if err := db.QueryRow("SELECT count(*) FROM orders").Scan(&stored); err != nil {
		return false, fmt.Sprintf("count orders: %v", err)
	}
	for _, row := range rows {
		if row.id == id {
			if len(rows) == stored {
				return true, fmt.Sprintf("order %d visible, %d rows match the database", id, stored)
			}
			return false, fmt.Sprintf("order %d visible but the list holds %d of %d rows", id, len(rows), stored)
		}
	}
	return false, fmt.Sprintf("newest order %d missing from the served list", id)
}

// ordersHandler serves the order list with the positions of every order.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	rows, used, err := listOrders()
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
		return
	}
	state.SetQueriesPerRequest(queries.observe(used))
	log.Printf("orders list served: rows=%d round_trips=%d", len(rows), used)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"count":%d,"queries":%d,"orders":[`, len(rows), used)
	for i, row := range rows {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"id":%d,"customer":%q,"total":%.2f,"skus":[`, row.id, row.customer, row.total)
		for j, sku := range row.skus {
			if j > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `%q`, sku)
		}
		fmt.Fprint(w, `]}`)
	}
	fmt.Fprint(w, `]}`)
}

// listOrders loads the orders and, for every order, the positions that belong
// to it. It returns the rows and the number of database round trips it used.
func listOrders() ([]orderRow, int, error) {
	used := 1
	rows, err := db.Query("SELECT id, customer, total FROM orders ORDER BY id" + limitClause(ordersLimit()))
	if err != nil {
		return nil, used, err
	}
	var out []orderRow
	for rows.Next() {
		var row orderRow
		if err := rows.Scan(&row.id, &row.customer, &row.total); err != nil {
			rows.Close()
			return nil, used, err
		}
		row.skus, err = positionsOf(row.id)
		if err != nil {
			rows.Close()
			return nil, used, err
		}
		used++
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, used, err
	}
	rows.Close()
	return out, used, nil
}

// positionsOf loads the positions of one order. It costs one round trip.
func positionsOf(orderID int) ([]string, error) {
	rows, err := db.Query("SELECT sku FROM order_positions WHERE order_id = $1", orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var skus []string
	for rows.Next() {
		var sku string
		if err := rows.Scan(&sku); err != nil {
			return nil, err
		}
		skus = append(skus, sku)
	}
	return skus, rows.Err()
}
