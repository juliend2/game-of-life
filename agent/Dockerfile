# Build context for this Dockerfile is the repo root, not the agent/ directory,
# because the binary depends on internal/spawn.
#
#   docker build -f agent/Dockerfile -t localhost:5000/gol-agent:latest .

# Stage 1 — build
FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY internal/ ./internal/
COPY agent/ ./agent/

# CGO_ENABLED=0  — no C bindings, fully static binary
# GOOS=linux     — target OS (important if building on macOS)
RUN CGO_ENABLED=0 GOOS=linux go build -o /agent ./agent

# Stage 2 — run
FROM scratch

COPY --from=builder /agent /agent
# TLS roots so the agent can call the Kubernetes API over HTTPS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENTRYPOINT ["/agent"]
