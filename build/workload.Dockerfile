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
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/incidentsim-workload"]
