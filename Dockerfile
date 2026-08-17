# Multi-stage Dockerfile for TaskEngine Server
FROM golang:1.24-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git make ca-certificates

COPY src/taskengine/go.mod src/taskengine/go.sum ./src/taskengine/
WORKDIR /src/src/taskengine
RUN go mod download

COPY src/taskengine/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/taskengine ./cmd/taskengine

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata curl

WORKDIR /app
COPY --from=builder /bin/taskengine /app/taskengine

# Default tasks configuration directory and data directory
COPY tasks/ /app/tasks/

EXPOSE 8080

VOLUME ["/app/data", "/app/tasks"]

ENTRYPOINT ["/app/taskengine"]
CMD ["server", "--port", "8080", "--tasks-dir", "/app/tasks", "--db-path", "/app/data/taskengine.db"]
