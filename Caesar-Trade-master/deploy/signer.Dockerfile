# ──────────────────────────────────────────────────────────────
# Caesar Signer — multi-stage build
# Produces a minimal scratch image with a statically-linked binary.
#
# IMPORTANT: The signer binary calls mlock(2) via memguard.
# The container MUST be granted CAP_IPC_LOCK at runtime:
#   docker run --cap-add IPC_LOCK ...
# or in docker-compose.yml:
#   cap_add: [IPC_LOCK]
# ──────────────────────────────────────────────────────────────

# ── Build stage ──────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

# CGO is required by memguard (mlock/memcall). build-base provides gcc/musl.
RUN apk add --no-cache build-base

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary via musl — compatible with scratch.
RUN CGO_ENABLED=1 go build \
    -ldflags='-s -w -extldflags "-static"' \
    -o /signer ./cmd/signer

# Create a non-root user entry for the scratch stage.
RUN echo "signer:x:10001:10001::/:" > /etc/signer-passwd

# ── Final stage ──────────────────────────────────────────────
FROM scratch

# Import CA certs for any future TLS needs (KMS, etc.).
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Import the non-root user.
COPY --from=builder /etc/signer-passwd /etc/passwd

# Copy the statically-linked binary.
COPY --from=builder /signer /signer

# Socket lives directly in /tmp (world-writable, no sub-directory needed).
VOLUME ["/tmp"]

# Run as non-root.
USER 10001

ENTRYPOINT ["/signer"]
