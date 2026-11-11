FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY app/ ./app/
RUN CGO_ENABLED=0 go build -o /app ./app/

FROM alpine:3.18
RUN apk --no-cache add ca-certificates
COPY --from=builder /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]