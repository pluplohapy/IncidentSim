#!/usr/bin/env bash
# Scaffold the non-secret parts of a new IncidentSim task.
set -euo pipefail

if [[ $# -ne 1 || ! "$1" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]]; then
  echo "usage: $0 task-id (lowercase kebab-case)" >&2
  exit 2
fi

id="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"
for dir in "manifests/$id.yaml" "scenarios/$id/workspace" "scenarios/$id/reference"; do
  if [[ -e "$root/$dir" ]]; then
    echo "refusing to overwrite $dir" >&2
    exit 1
  fi
done

mkdir -p "$root/manifests" "$root/scenarios/$id/workspace" "$root/scenarios/$id/reference"
cat > "$root/manifests/$id.yaml" <<EOF
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: $id
  title: "Новая code-задача: $id"
  level: code
  tags: [go, postgres]
  estimated_minutes: 30
spec:
  workspace: scenarios/$id/workspace
  reference: scenarios/$id/reference
  runtime:
    engine: compose
    memory_mib: 1024
  services:
    - name: db
      image: postgres:16-alpine
      env:
        POSTGRES_PASSWORD: incidentsim
        POSTGRES_DB: orders
    - name: app
      image: incidentsim/workload:dev
      env:
        DB_HOST: db
        DB_PORT: "5432"
        DB_NAME: orders
        DB_USER: postgres
        DB_PASSWORD: incidentsim
      ports: [8080]
      healthcheck:
        path: /healthz
        port: 8080
        interval_seconds: 5
        timeout_seconds: 2
  editable: [app/handler.go]
  symptoms:
    - id: symptom
      observation: "TODO: observable symptom"
  hints:
    - level: 1
      text: "TODO: first diagnostic clue"
    - level: 2
      text: "TODO: second diagnostic clue"
    - level: 3
      text: "TODO: root-cause direction"
  fault:
    type: sql_config
    target: db
    trigger: on_start
    options:
      statements: ["CREATE TABLE IF NOT EXISTS orders (id serial primary key)"]
  verify:
    correctness:
      - id: healthz-200
        type: http
        service: app
        path: /healthz
        expect:
          status: 200
    quality:
      - id: artifact-check
        type: exec
        service: app
        command: ["sh", "-c", "true"]
        reason: "TODO: prove the root cause is fixed in the artifact"
      - id: behavior-check
        type: replay
        field: error_rate
        expect:
          max: 0.005
        reason: "TODO: prove the behavior remains healthy under load"
  root_cause:
    summary: "TODO: root cause"
    layer: code
  distractors:
    - id: bigger-pool
      title: "Увеличить лимит пула"
      symptom: symptom
      masked_artifact: "TODO: what remains broken"
      fails: artifact-check
      apply:
        kind: env
        service: app
        vars:
          POOL_MAX: "100"
  replay:
    profile: workloads/$id.yaml
    duration: 10s
    rps: 10
  reference_solution:
    summary: "TODO: safe production fix"
    steps: []
EOF

# Fill the workspace with the shared, non-editable stand files. The generated
# handler remains the only participant-owned Go file.
"$root/scripts/sync-stand.sh" "$id"
cat > "$root/scenarios/$id/workspace/Dockerfile" <<'EOF'
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY app/ ./app/
RUN CGO_ENABLED=0 go build -o /app ./app/
FROM alpine:3.20
COPY --from=builder /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
EOF
mkdir -p "$root/scenarios/$id/reference/app"
cp "$root/scenarios/$id/workspace/app/handler.go" "$root/scenarios/$id/reference/app/handler.go"
echo "created manifests/$id.yaml and scenarios/$id/{workspace,reference}"
echo "replace TODO values, add workloads/$id.yaml, then run: go run ./cmd/incidentsim validate manifests"
