package main

import "net/http"

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unimplemented handler"}`, http.StatusInternalServerError)
	})
}
