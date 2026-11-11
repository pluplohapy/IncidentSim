#!/usr/bin/env bash
#
# scripts/sync-stand.sh — generate the task workspace from the shared stand.
#
# Copies internal/stand/*.go into scenarios/<id>/workspace/app/ (as package
# main) and leaves exactly the task-editable files (app/handler.go and
# app/migrations/*.sql). Everything else in app/ comes from the shared stand
# and must not be modified by the participant. CI verifies that this script
# plus `git diff --exit-code` is clean.
#
# Usage:
#   ./scripts/sync-stand.sh                 # regenerate all workspaces
#   ./scripts/sync-stand.sh <task-id>       # regenerate one task
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STAND_DIR="internal/stand"
TASKS=()
if [ $# -gt 0 ]; then
    TASKS=("$1")
else
    for d in scenarios/*/; do
        TASKS+=("$(basename "$d")")
    done
fi

for task in "${TASKS[@]}"; do
    ws="scenarios/$task/workspace"
    app="$ws/app"
    migrations="$ws/app/migrations"

    echo "== $task: syncing stand to $ws =="

    mkdir -p "$app"

    # Copy the shared stand into app/ as package main.
    for f in "$STAND_DIR"/*.go; do
        base="$(basename "$f")"
        # Rewrite package stand -> package main so the file compiles inside the
        # app package. These files are generated and must be refreshed on every
        # run; only handler.go and migrations are participant-owned.
        sed 's/^package stand$/package main/' "$f" > "$app/$base"
    done

    # Keep existing handler.go if present; otherwise write a minimal stub
    # that returns 500 so the task starts in a failing state.
    if [ ! -f "$app/handler.go" ]; then
        cat > "$app/handler.go" <<'EOF'
package main

import (
	"fmt"
	"net/http"
)

// handlerRoutes registers the task-specific HTTP routes. The participant
// edits this file to fix the incident.
var handlerRoutes = func(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unimplemented handler"}`, http.StatusInternalServerError)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
}
EOF
    fi

    # Migrations are task-specific SQL; keep existing ones.
    mkdir -p "$migrations"

    # go.mod/go.sum must exist for the Builder. Create minimal ones if absent.
    if [ ! -f "$ws/go.mod" ]; then
        cat > "$ws/go.mod" <<'EOF'
module app

go 1.24

require github.com/lib/pq v1.10.9
EOF
        touch "$ws/go.sum"
    fi

    echo "== $task: done =="
done
echo "sync-stand: all workspaces rendered"
