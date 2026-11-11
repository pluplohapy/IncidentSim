package main

// Cache-everything distractor: every answer, found or absent, is remembered
// forever. The database load from the penetration goes away, and so does the
// freshness of a read after a write: a created order keeps answering 404.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func envSeconds(key string, fallback int) time.Duration {
	if raw := os.Getenv(key); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Duration(fallback) * time.Second
}

// cacheEntry is one rendered answer plus the status it was served with.
type cacheEntry struct {
	body      string
	status    int
	expiresAt time.Time
}

var (
	cacheMu    sync.Mutex
	cacheStore = map[string]cacheEntry{}
)

func orderKey(id int) string { return "order:" + strconv.Itoa(id) }

// cacheGet returns an entry that is still considered fresh. An entry stored
// forever has no expiry, so it never goes stale.
func cacheGet(key string) (cacheEntry, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := cacheStore[key]
	if !ok || !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

// cachePutForever stores an answer with no expiry at all.
func cachePutForever(key string, entry cacheEntry) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheStore[key] = entry
	state.SetCacheEntries(int64(len(cacheStore)))
}

type orderDetail struct {
	ID       int     `json:"id"`
	Customer string  `json:"customer"`
	Total    float64 `json:"total"`
	Events   int     `json:"events"`
	Source   string  `json:"source"`
}

func resolveOrder(id int) (orderDetail, bool, error) {
	var detail orderDetail
	var found bool
	row := db.QueryRow("SELECT id, customer, total FROM orders WHERE id = $1", id)
	switch err := row.Scan(&detail.ID, &detail.Customer, &detail.Total); {
	case err == nil:
		found, detail.Source = true, "active"
	case err == sql.ErrNoRows:
	default:
		return orderDetail{}, false, err
	}
	var archived orderDetail
	switch err := db.QueryRow("SELECT id, customer, total FROM orders_archive WHERE id = $1", id).
		Scan(&archived.ID, &archived.Customer, &archived.Total); {
	case err == nil:
		if !found {
			detail, found, detail.Source = archived, true, "archive"
		}
	case err == sql.ErrNoRows:
	default:
		return orderDetail{}, false, err
	}
	var events int
	if err := db.QueryRow("SELECT count(*) FROM order_events WHERE order_id = $1", id).Scan(&events); err != nil {
		return orderDetail{}, false, err
	}
	detail.Events = events
	return detail, found, nil
}

func loadOrder(id int) (cacheEntry, error) {
	detail, found, err := resolveOrder(id)
	if err != nil {
		return cacheEntry{}, err
	}
	if !found {
		return cacheEntry{status: http.StatusNotFound, body: `{"error":"order not found"}`}, nil
	}
	body, err := json.Marshal(detail)
	if err != nil {
		return cacheEntry{}, err
	}
	return cacheEntry{status: http.StatusOK, body: string(body)}, nil
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		createOrderHandler(w, r)
		return
	}
	state.incRequests()
	id, ok := orderID(r.URL.Path)
	if !ok {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}
	key := orderKey(id)
	if entry, ok := cacheGet(key); ok {
		state.AddCacheHits(1)
		if entry.status != http.StatusOK {
			log.Printf("negative cache hit key=%s", key)
		}
		writeEntry(w, entry)
		return
	}
	entry, err := loadOrder(id)
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"lookup failed"}`, http.StatusInternalServerError)
		return
	}
	cachePutForever(key, entry)
	writeEntry(w, entry)
}

// createOrderHandler inserts the row and, like the rest of this variant, does
// not touch the cache: whatever was cached for the id stays cached.
func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	var order struct {
		ID       int     `json:"id"`
		Customer string  `json:"customer"`
		Total    float64 `json:"total"`
	}
	if err := json.Unmarshal(payload, &order); err != nil || order.Customer == "" {
		http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
		return
	}
	var id int
	if order.ID > 0 {
		err = db.QueryRow("INSERT INTO orders (id, customer, total) VALUES ($1, $2, $3) RETURNING id",
			order.ID, order.Customer, order.Total).Scan(&id)
	} else {
		err = db.QueryRow("INSERT INTO orders (customer, total) VALUES ($1, $2) RETURNING id",
			order.Customer, order.Total).Scan(&id)
	}
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"insert failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"id":%d}`, id)
}

func writeEntry(w http.ResponseWriter, entry cacheEntry) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(entry.status)
	fmt.Fprint(w, entry.body)
}

func orderID(path string) (int, bool) {
	rest, ok := strings.CutPrefix(path, "/api/orders/")
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func serveOrder(id int) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ordersHandler(rec, httptest.NewRequest(http.MethodGet, "/api/orders/"+strconv.Itoa(id), nil))
	return rec
}

// checkInvariant answers /debug/invariant/<name>. "freshness" creates an order
// behind an id that was absent a moment ago and requires the next read to see
// it immediately.
func checkInvariant(name string) (bool, string) {
	if name != "freshness" {
		return false, "unknown invariant"
	}
	var id int
	if err := db.QueryRow("SELECT COALESCE(max(id), 0) + 1000 FROM orders").Scan(&id); err != nil {
		return false, "cannot reserve a free id"
	}
	marker := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	if rec := serveOrder(id); rec.Code != http.StatusNotFound {
		return false, "the probed id is not absent"
	}
	body := fmt.Sprintf(`{"id":%d,"customer":%q,"total":7}`, id, marker)
	rec := httptest.NewRecorder()
	ordersHandler(rec, httptest.NewRequest(http.MethodPost, "/api/orders", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		return false, "the create was not accepted"
	}
	created := serveOrder(id)
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), marker) {
		return false, "the created order is not visible on the next read"
	}
	return true, "the created order is visible immediately"
}

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders/", ordersHandler)
	mux.Handle("/debug/invariant/", InvariantHandler(checkInvariant))
}
