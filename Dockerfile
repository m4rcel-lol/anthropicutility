# syntax=docker/dockerfile:1

# --- builder: compile a static binary ---
FROM golang:1.22-bookworm AS builder

WORKDIR /src

# Cache module downloads
COPY go.mod go.sum* ./
RUN go mod download

COPY *.go ./
COPY assets/ ./assets/
# CGO_ENABLED=0 + modernc.org/sqlite = pure-Go static binary, no libc dependency.
# go:embed pulls assets/icon.jpg into the binary at compile time.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bot .

# --- runtime: scratch keeps the image tiny; no shell, no package manager ---
FROM scratch

COPY --from=builder /out/bot /bot

# SQLite file lives on the named volume mounted at /data.
VOLUME ["/data"]

# No USER directive: scratch has no /etc/passwd; run as whatever the orchestrator sets.
ENTRYPOINT ["/bot"]
