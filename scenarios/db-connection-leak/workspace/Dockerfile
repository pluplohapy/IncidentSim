# Canonical Dockerfile of a code-level task workspace.
#
# It documents how the service image is produced and lets `incidentsim
# validate` assert the workspace is self-contained. The Builder itself
# compiles workspace/app in a throw-away container and installs the binary in
# a thin image; this file is the human-readable equivalent.
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY app/ ./app/
RUN CGO_ENABLED=0 go build -trimpath -o /app ./app/

FROM alpine:3.20
RUN apk --no-cache add ca-certificates
COPY --from=builder /app /usr/local/bin/app
RUN mkdir -p /app/migrations
WORKDIR /
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/app"]
