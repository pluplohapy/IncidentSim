# Builds the simulated workload service used inside task environments.
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

FROM alpine:3.20
RUN apk add --no-cache curl postgresql-client
COPY --from=build /out/incidentsim-workload /usr/local/bin/incidentsim-workload
# The declared log path must be writable by the unprivileged runtime user,
# otherwise "where are the logs" has no answer inside the container.
RUN mkdir -p /var/log/app && chown 10001:10001 /var/log/app
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/incidentsim-workload"]
