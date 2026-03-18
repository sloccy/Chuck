FROM golang:1.25-alpine AS builder

WORKDIR /app

# Install wget for downloading jszip
RUN apk add --no-cache wget

# Download JSZip
RUN wget -q -O /tmp/jszip.min.js \
    https://cdnjs.cloudflare.com/ajax/libs/jszip/3.10.1/jszip.min.js

# Copy go module files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Replace placeholder with real JSZip
RUN cp /tmp/jszip.min.js static/jszip.min.js

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o chuck .

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/chuck .

# Data directory for users.json
RUN mkdir -p /data

ENV PORT=8080 \
    DATA_DIR=/data \
    ADMIN_EMAIL=admin@localhost \
    ADMIN_PASSWORD=changeme

EXPOSE 8080

VOLUME ["/data"]

CMD ["./chuck"]
