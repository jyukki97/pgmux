# Stage 1: Build
FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /db-proxy ./cmd/db-proxy

# Stage 2: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /db-proxy /usr/local/bin/db-proxy

EXPOSE 5432 9090 9091
ENTRYPOINT ["db-proxy"]
CMD ["config.yaml"]
