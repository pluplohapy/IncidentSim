package main

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
	"sync/atomic"
	"time"
)

// cacheTTL is how long one cached order answer stays fresh, negativeTTL how
// long an "not found" answer may be trusted. Both come from the environment.
var (
	cacheTTL    = envSeconds("CACHE_TTL_SECONDS", 5)
	negativeTTL = envSeconds("NEGATIVE_TTL_SECONDS", 5)
	// orderQueries is the cost of one order lookup: the resolver checks the
	// active table, the archive and the event history before it answers, so a
	// lookup is never a single statement.
	orderQueries = 3
	// qprAlpha is the weight of the newest observation in the
	// queries-per-request EWMA the stand publishes on /metrics.
	qprAlpha = 0.35
)

func envSeconds(key string, fallback int) time.Duration {
	if raw := os.Getenv(key); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Duration(fallback) * time.Second
}

// cacheEntry is one rendered answer plus the status it was served with. A
// cached "not found" is a negative entry: it keeps the database from being
// asked the same absent id over and over.
type cacheEntry struct {
	body      string
	status    int
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

func orderKey(id int) string { return "order:" + strconv.Itoa(id) }

// cacheGet returns a fresh answer for key, whatever status it carries.
func cacheGet(key string) (cacheEntry, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := cacheStore[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

// cachePut stores one found answer for cacheTTL.
func cachePut(key string, entry cacheEntry) {
	entry.expiresAt = time.Now().Add(cacheTTL)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheStore[key] = entry
	state.SetCacheEntries(int64(len(cacheStore)))
}

// cachePutNegative remembers an absent answer for negativeTTL: the next read
// of the same id is served from memory instead of hitting the database.
func cachePutNegative(key string, entry cacheEntry) {
	entry.expiresAt = time.Now().Add(negativeTTL)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheStore[key] = entry
	state.SetCacheEntries(int64(len(cacheStore)))
}

// cacheInvalidate drops whatever the cache believed about one id, so the next
// read goes back to the database.
func cacheInvalidate(key string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	delete(cacheStore, key)
	state.SetCacheEntries(int64(len(cacheStore)))
}

// herdEnter records one in-flight lookup of key and returns how many lookups
// of that key are running right now.
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

// observeQueries folds the statements one request actually caused into the
// queries-per-request EWMA.
func observeQueries(n int) {
	qprMu.Lock()
	qprValue = qprValue*(1-qprAlpha) + float64(n)*qprAlpha
	value := qprValue
	qprMu.Unlock()
	state.SetQueriesPerRequest(value)
}

// orderDetail is the answer of one resolved order.
type orderDetail struct {
	ID       int     `json:"id"`
	Customer string  `json:"customer"`
	Total    float64 `json:"total"`
	Events   int     `json:"events"`
	Source   string  `json:"source"`
}

// resolveOrder looks for an order across its lifecycle tables. It checks the
// active table, the archive and the event history before it answers, because
// an order may live in any of them and only an exhausted lookup is a real
// "not found".
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

// loadOrder resolves one order and renders the answer that will be cached.
// A found order and an absent id are both worth remembering: an absent id is
// cached as a negative answer for its own, shorter TTL.
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

// ordersHandler serves one order: GET by id, POST to create one.
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
		observeQueries(0)
		if entry.status != http.StatusOK {
			log.Printf("negative cache hit key=%s", key)
		}
		writeEntry(w, entry)
		return
	}
	inflight := herdEnter(key)
	defer herdLeave(key)
	entry, err := loadOrder(id)
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"lookup failed"}`, http.StatusInternalServerError)
		return
	}
	ttl := cacheTTL
	if entry.status == http.StatusOK {
		cachePut(key, entry)
	} else {
		ttl = negativeTTL
		cachePutNegative(key, entry)
	}
	log.Printf("cache fill key=%s status=%d inflight=%d queries=%d ttl=%s",
		key, entry.status, inflight, orderQueries, ttl)
	observeQueries(orderQueries)
	writeEntry(w, entry)
}

// createOrderHandler inserts one order and drops whatever the cache believed
// about that id, so the very next read sees the new row.
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
	cacheInvalidate(orderKey(id))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"id":%d}`, id)
}

func writeEntry(w http.ResponseWriter, entry cacheEntry) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(entry.status)
	fmt.Fprint(w, entry.body)
}

// orderID pulls the numeric id out of /api/orders/<id>.
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
