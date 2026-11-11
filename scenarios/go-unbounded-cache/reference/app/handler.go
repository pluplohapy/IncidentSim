package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/cache", cacheHandler)
}

// cacheLimit bounds how many rendered views the service keeps in memory.
const cacheLimit = 200

// viewCache holds the rendered view of every record the service answered for.
var (
	viewMu    sync.Mutex
	viewCache = map[string]string{}
	viewOrder []string // insertion order of the keys, oldest first
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
	for _, id := range ids {
		remember(fmt.Sprintf("record:%d", id), renderView(id, term))
	}
	viewMu.Lock()
	entries := len(viewCache)
	viewMu.Unlock()
	state.incDBQueries()
	state.SetCacheEntries(int64(entries))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"cached_views":%d}`, entries)
}

// remember stores the rendered view of one record and releases the oldest one
// once the cache is full, so the memory it holds stays bounded.
func remember(key, view string) {
	viewMu.Lock()
	defer viewMu.Unlock()
	if _, ok := viewCache[key]; !ok {
		viewOrder = append(viewOrder, key)
	}
	viewCache[key] = view
	for len(viewCache) > cacheLimit {
		delete(viewCache, viewOrder[0])
		viewOrder = viewOrder[1:]
	}
}

// renderView builds the cached view of one record: a fixed-size document, so
// every entry of the map costs the same amount of memory.
func renderView(id int, term string) string {
	return strings.Repeat(fmt.Sprintf("view:%d:%s:", id, term), 350)
}
