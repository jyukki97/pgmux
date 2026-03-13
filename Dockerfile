# Stage 1: Build
FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -o /pgmux ./cmd/pgmux

# Stage 2: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /pgmux /usr/local/bin/pgmux

EXPOSE 5432 9090 9091
ENTRYPOINT ["pgmux"]
CMD ["config.yaml"]
