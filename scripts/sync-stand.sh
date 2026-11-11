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
# The same generation runs for scenarios/<id>/reference/app so the answer key
# compiles inside the repository module and differs from the workspace only in
# the editable handler.
#
# Usage:
#   ./scripts/sync-stand.sh                 # regenerate all workspaces
#   ./scripts/sync-stand.sh <task-id>       # regenerate one task
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STAND_DIR="internal/stand"
TEMPLATE_DIR="$STAND_DIR/app-template"
TASKS=()
if [ $# -gt 0 ]; then
    TASKS=("$1")
else
    for d in scenarios/*/; do
        TASKS+=("$(basename "$d")")
    done
fi

# Generate the app package of one directory (workspace or reference).
# $1 = absolute app dir, $2 = label for logs.
generate_app() {
    local app="$1" label="$2"

    mkdir -p "$app"
    for f in "$STAND_DIR"/*.go; do
        local base
        base="$(basename "$f")"
        # Rewrite package stand -> package main so the file compiles inside
        # the app package. These files are generated and must be refreshed on
        # every run; only handler.go and migrations are participant-owned.
        sed 's/^package stand$/package main/' "$f" > "$app/$base"
    done
    # The thin generated entrypoint: everything else is stand code.
    cp "$TEMPLATE_DIR/main.go" "$app/main.go"
}

for task in "${TASKS[@]}"; do
    ws="scenarios/$task/workspace"
    app="$ws/app"
    migrations="$ws/app/migrations"

    echo "== $task: syncing stand to $ws =="

    if [ ! -d "$ws" ]; then
        echo "== $task: no workspace directory, skipping =="
        continue
    fi

    generate_app "$app" workspace

    # Keep existing handler.go if present; otherwise write a minimal stub
    # that answers 500 so the task starts in a failing state. The stub is a
    # placeholder: scripts/new-task.sh plus the task author replace it.
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

    # go.mod/go.sum/Dockerfile are canonical: the participant's fix must not
    # have to change them, so they are regenerated on every run.
    cp "$TEMPLATE_DIR/go.mod" "$ws/go.mod"
    cp "$TEMPLATE_DIR/go.sum" "$ws/go.sum"
    cp "$TEMPLATE_DIR/Dockerfile" "$ws/Dockerfile"

    # Reference: the same generated stand plus the fixed handler, so the
    # answer key compiles inside the repository module. Only code-level tasks
    # carry a Go reference package: a config/db/composite answer is a file or
    # a statement, not a handler.
    ref="scenarios/$task/reference"
    ref_level="$(sed -n 's/^[[:space:]]*level:[[:space:]]*//p' "manifests/$task.yaml" 2>/dev/null | head -1)"
    if [ -d "$ref" ] && { [ "$ref_level" = "code" ] || [ -f "$ref/app/handler.go" ]; }; then
        generate_app "$ref/app" reference
    fi

    echo "== $task: done =="
done
echo "sync-stand: all workspaces rendered"
