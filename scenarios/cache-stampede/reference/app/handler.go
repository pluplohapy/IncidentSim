package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// cacheTTL is the lifetime of one cached orders snapshot. Operations tunes it
// through the environment; the default keeps the snapshot hot under load and
// bounds how far a read may lag behind a write.
var cacheTTL = func() time.Duration {
	if raw := os.Getenv("CACHE_TTL_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 2 * time.Second
}()

// qprAlpha is the weight of the newest observation in the queries-per-request
// EWMA the stand publishes on /metrics.
const qprAlpha = 0.45

// snapshotQueries is the cost of one summary reload in database statements.
const snapshotQueries = 5

// fillGroup deduplicates the snapshot load of one key.
var fillGroup singleflight.Group

// cacheEntry is one rendered snapshot plus the moment it stops being fresh.
type cacheEntry struct {
	body      string
	expiresAt time.Time
}

var (
	cacheMu    sync.Mutex
	cacheStore = map[string]cacheEntry{}
	herdMu     sync.Mutex
	herdCount  = map[string]*int64{}
	qprMu      sync.Mutex
	qprValue   float64
)

// cacheGet returns a fresh snapshot of key. A missing or expired entry is a
// miss: the caller reloads it.
func cacheGet(key string) (string, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := cacheStore[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.body, true
}

// cachePut stores a snapshot that stays fresh for cacheTTL, with the expiry
// jittered by ±20% so a fleet of keys does not expire at the same instant.
func cachePut(key, body string) {
	ttl := time.Duration(float64(cacheTTL) * (0.8 + 0.4*rand.Float64()))
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheStore[key] = cacheEntry{body: body, expiresAt: time.Now().Add(ttl)}
	state.SetCacheEntries(int64(len(cacheStore)))
}

// herdEnter records one in-flight load of key and returns how many loads of
// that key are running right now. The number is what makes an uncoordinated
// cache observable: a coordinated load always reports one.
func herdEnter(key string) int64 {
	herdMu.Lock()
	counter, ok := herdCount[key]
	if !ok {
		counter = new(int64)
		herdCount[key] = counter
	}
	herdMu.Unlock()
	return atomic.AddInt64(counter, 1)
}

func herdLeave(key string) {
	herdMu.Lock()
	counter := herdCount[key]
	herdMu.Unlock()
	if counter != nil {
		atomic.AddInt64(counter, -1)
	}
}

// observeQueries folds the database statements one request actually caused
// into the queries-per-request EWMA.
func observeQueries(n int) {
	qprMu.Lock()
	qprValue = qprValue*(1-qprAlpha) + float64(n)*qprAlpha
	value := qprValue
	qprMu.Unlock()
	state.SetQueriesPerRequest(value)
}

// loadOrdersSnapshot reads the dashboard summary of the orders table. The
// summary is five statements, so one reload is not free.
func loadOrdersSnapshot() (string, error) {
	var count int
	if err := db.QueryRow("SELECT count(*) FROM orders").Scan(&count); err != nil {
		return "", err
	}
	var revenue float64
	if err := db.QueryRow("SELECT COALESCE(SUM(total), 0) FROM orders").Scan(&revenue); err != nil {
		return "", err
	}
	var topCustomer string
	if err := db.QueryRow("SELECT customer FROM orders ORDER BY total DESC, id ASC LIMIT 1").Scan(&topCustomer); err != nil {
		return "", err
	}
	recent, err := recentCustomers()
	if err != nil {
		return "", err
	}
	var distinct int
	if err := db.QueryRow("SELECT count(DISTINCT customer) FROM orders").Scan(&distinct); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"orders_count":       count,
		"revenue":            revenue,
		"top_customer":       topCustomer,
		"recent_customers":   recent,
		"distinct_customers": distinct,
	})
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func recentCustomers() ([]string, error) {
	rows, err := db.Query("SELECT customer FROM orders ORDER BY id DESC LIMIT 5")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recent := []string{}
	for rows.Next() {
		var customer string
		if err := rows.Scan(&customer); err != nil {
			return nil, err
		}
		recent = append(recent, customer)
	}
	return recent, rows.Err()
}

// loadAndCache returns a fresh snapshot of key, reading the database when the
// cached entry expired. Every caller that arrives while the entry is expired
// waits for one shared read instead of issuing its own.
func loadAndCache(key string) (string, int, error) {
	value, err, shared := fillGroup.Do(key, func() (any, error) {
		inflight := herdEnter(key)
		defer herdLeave(key)
		log.Printf("cache fill key=%s herd=%d", key, inflight)
		body, err := loadOrdersSnapshot()
		if err != nil {
			return nil, err
		}
		cachePut(key, body)
		return body, nil
	})
	if err != nil {
		return "", 0, err
	}
	loaded, _ := value.(string)
	if shared {
		return loaded, 0, nil
	}
	return loaded, snapshotQueries, nil
}

// ordersHandler serves the orders summary: the cached snapshot while it is
// fresh, a database reload once the TTL elapsed. POST appends one order.
func ordersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		createOrderHandler(w, r)
		return
	}
	state.incRequests()
	if body, ok := cacheGet("orders"); ok {
		state.AddCacheHits(1)
		observeQueries(0)
		writeBody(w, body)
		return
	}
	body, queries, err := loadAndCache("orders")
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	observeQueries(queries)
	writeBody(w, body)
}

// createOrderHandler appends one order. The snapshot cache is refreshed by the
// TTL, so the write becomes visible on the next reload.
func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	var order struct {
		Customer string  `json:"customer"`
		Total    float64 `json:"total"`
	}
	if err := json.Unmarshal(payload, &order); err != nil || order.Customer == "" {
		http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
		return
	}
	var id int
	if err := db.QueryRow("INSERT INTO orders (customer, total) VALUES ($1, $2) RETURNING id",
		order.Customer, order.Total).Scan(&id); err != nil {
		state.incErrors()
		http.Error(w, `{"error":"insert failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"id":%d}`, id)
}

func writeBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

// checkInvariant answers /debug/invariant/<name>. "freshness" writes an order
// and requires the served snapshot to observe it inside the TTL window.
func checkInvariant(name string) (bool, string) {
	if name != "freshness" {
		return false, "unknown invariant"
	}
	marker := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	if rec := serveOrders(); rec.Code != http.StatusOK {
		return false, "warmup read failed"
	}
	write := httptest.NewRequest(http.MethodPost, "/api/orders",
		strings.NewReader(fmt.Sprintf(`{"customer":%q,"total":1}`, marker)))
	rec := httptest.NewRecorder()
	ordersHandler(rec, write)
	if rec.Code != http.StatusCreated {
		return false, "write was not accepted"
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if body := serveOrders().Body.String(); strings.Contains(body, marker) {
			return true, "the served snapshot observed the write"
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false, "the served snapshot did not observe the write inside the TTL window"
}

func serveOrders() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ordersHandler(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	return rec
}

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", ordersHandler)
	mux.Handle("/debug/invariant/", InvariantHandler(checkInvariant))
}
