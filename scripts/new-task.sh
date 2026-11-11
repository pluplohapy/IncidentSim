#!/usr/bin/env bash
# Scaffold the non-secret parts of a new IncidentSim task.
set -euo pipefail

if [[ $# -ne 1 || ! "$1" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]]; then
  echo "usage: $0 task-id (lowercase kebab-case)" >&2
  exit 2
fi

id="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"
for dir in "manifests/$id" "scenarios/$id/workspace" "scenarios/$id/reference"; do
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
  title: "TODO: title"
  difficulty: intermediate
spec:
  level: config
  engine: compose
  workspace: scenarios/$id/workspace
  reference: scenarios/$id/reference
  editable:
    - config/app.conf
  symptoms:
    - id: symptom
      description: "TODO: observable symptom"
  root_cause:
    summary: "TODO: root cause"
    clues: []
  verify:
    correctness:
      - id: correctness
        description: "TODO: service recovers"
        command: "true"
    quality:
      - id: quality
        description: "TODO: root cause is fixed"
        command: "true"
  distractors:
    - id: distractor
      title: "TODO: symptom-only workaround"
      symptom: symptom
      masked_artifact: "TODO: what remains broken"
      fails: quality
      apply:
        kind: env
        service: app
        vars:
          TODO: "replace-me"
  reference_solution:
    summary: "TODO: safe production fix"
    steps: []
EOF
echo "created manifests/$id.yaml and scenarios/$id/{workspace,reference}"
echo "fill every TODO, add a real service, then run: go run ./cmd/incidentsim validate manifests"
