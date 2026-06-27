# Build context for this Dockerfile is the repo root, not the perceiver/ directory,
# because the binary depends on internal/spawn.
#
#   docker build -f perceiver/Dockerfile -t localhost:5000/gol-perceiver:latest .

# Stage 1 — build
FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY internal/ ./internal/
COPY perceiver/ ./perceiver/

RUN CGO_ENABLED=0 GOOS=linux go build -o /perceiver ./perceiver

# Stage 2 — run
FROM scratch

COPY --from=builder /perceiver /perceiver
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENTRYPOINT ["/perceiver"]
