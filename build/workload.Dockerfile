# Builds the task workload image used inside task environments.
#
# The image carries three binaries:
#
#   incidentsim-workload  the legacy simulated service (kept until the last
#                         config/db/composite task moves to the real stand)
#   incidentsim-depstub   the controllable dependency of the `dep` stack
#                         (Part E: POST /control steers error_rate, latency,
#                         down and hang)
#   app                   the placeholder entrypoint; code-level tasks replace
#                         it with the binary built from the workspace
#
# The same image is used by the compose engine (as a compose service) and by
# the Firecracker engine (as the rootfs layer), so a task behaves identically
# on both.
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/incidentsim-workload ./cmd/incidentsim-workload
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/incidentsim-depstub ./cmd/incidentsim-depstub

FROM alpine:3.20
RUN apk add --no-cache curl postgresql-client
COPY --from=build /out/incidentsim-workload /usr/local/bin/incidentsim-workload
COPY --from=build /out/incidentsim-depstub /usr/local/bin/incidentsim-depstub
# The declared log path must be writable by the unprivileged runtime user,
# otherwise "where are the logs" has no answer inside the container.
RUN mkdir -p /var/log/app && chown 10001:10001 /var/log/app
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/incidentsim-workload"]
