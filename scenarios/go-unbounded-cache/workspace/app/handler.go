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
	if strings.EqualFold(os.Getenv("CACHE_EVICT"), "timer") {
		go releaseCachePeriodically()
	}
	mux.HandleFunc("/api/cache", cacheHandler)
}

// viewCache holds the rendered view of every record the service answered for.
var (
	viewMu    sync.Mutex
	viewCache = map[string]string{}
)

// cacheHandler ingests a batch of records for the requested term and answers
// with the number of views the service currently holds.
func cacheHandler(w http.ResponseWriter, r *http.Request) {
	state.incRequests()
	term := r.URL.Query().Get("q")
	if term == "" {
		term = "all"
	}
	rows, err := db.Query(`INSERT INTO cache_records (term) SELECT $1 FROM generate_series(1, 20) RETURNING id`, term)
	if err != nil {
		state.incErrors()
		http.Error(w, `{"error":"ingest failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			state.incErrors()
			http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		state.incErrors()
		http.Error(w, `{"error":"rows error"}`, http.StatusInternalServerError)
		return
	}
	viewMu.Lock()
	for _, id := range ids {
		viewCache[fmt.Sprintf("record:%d", id)] = renderView(id, term)
	}
	entries := len(viewCache)
	viewMu.Unlock()
	state.incDBQueries()
	state.SetCacheEntries(int64(entries))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"cached_views":%d}`, entries)
}

// renderView builds the cached view of one record: a fixed-size document, so
// every entry of the map costs the same amount of memory.
func renderView(id int, term string) string {
	return strings.Repeat(fmt.Sprintf("view:%d:%s:", id, term), 350)
}

// releaseCachePeriodically empties the whole map once a minute. CACHE_EVICT=
// timer turns it on, and an observation window shorter than the period only
// ever sees the map grow.
func releaseCachePeriodically() {
	for range time.Tick(60 * time.Second) {
		viewMu.Lock()
		viewCache = map[string]string{}
		viewMu.Unlock()
	}
}
