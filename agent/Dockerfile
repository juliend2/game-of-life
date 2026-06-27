# Stage 1 — build
# Use the official Go image to compile a fully static binary.
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Copy dependency files first so Docker caches this layer.
# The binary only gets rebuilt when source changes, not deps.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0  — no C bindings, fully static binary
# GOOS=linux     — target OS (important if building on macOS)
RUN CGO_ENABLED=0 GOOS=linux go build -o /agent .

# Stage 2 — run
# scratch is an empty base image — just the binary, nothing else.
# Result: ~10 MB image instead of ~300 MB with a full Linux distro.
FROM scratch

# Copy the binary from the builder stage.
COPY --from=builder /agent /agent

# Copy TLS certificates so the agent can make HTTPS calls
# to the Kubernetes API (which uses HTTPS).
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENTRYPOINT ["/agent"]
