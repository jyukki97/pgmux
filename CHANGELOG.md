# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-03-17

### Added

- **Transaction-Level Connection Pooling** — Writer/Reader connection pools with `DISCARD ALL` session reset
- **Automatic Read/Write Routing** — `SELECT` to Reader, write queries (`INSERT`, `UPDATE`, `DELETE`, `MERGE`, `CALL`, DDL) to Writer
- **AST-Based Query Classification** — pg_query_go AST parser with fallback to string-based parser
  - Side-effectful SELECT detection (`nextval()`, `setval()`, `pg_advisory_lock()`, etc.)
  - `EXPLAIN ANALYZE` with write sub-query detection
  - CTE with write operations (`WITH ... AS (INSERT/UPDATE/DELETE)`)
  - Locking clause detection (`FOR UPDATE`, `FOR SHARE`, etc.)
- **Query Caching** — In-memory LRU cache with TTL, table-level invalidation, Redis Pub/Sub multi-instance sync
- **Semantic Cache Keys** — AST normalization for structural query deduplication
- **Prepared Statement Routing** — Extended Query Protocol support with Reader routing
- **Prepared Statement Multiplexing** — Parse/Bind interception, Safe Simple Query synthesis (type-specific literal serialization)
- **Replication Lag Handling** — Timer-based `read_after_write_delay` or LSN-based Causal Consistency
- **Query Firewall** — AST-based dangerous query blocking (DELETE/UPDATE without WHERE, DROP TABLE, TRUNCATE)
- **Hint-Based Routing** — SQL comment hints (`/* route:writer */`, `/* timeout:5s */`)
- **Transaction Awareness** — `BEGIN`~`COMMIT`/`ROLLBACK`/`ABORT` detection with Writer pinning
- **TLS Termination** — Optional TLS listener with cert/key configuration
- **PostgreSQL Authentication** — MD5 and SCRAM-SHA-256, optional proxy-level front-end auth
- **Multi-Database Routing** — Per-database Writer/Reader pools, balancers, and Circuit Breakers
- **Per-User/Per-Database Connection Limits** — PostgreSQL standard error code (53300) rejection
- **Session Compatibility Guard** — Detect LISTEN, SET, DECLARE, CREATE TEMP, PREPARE, advisory locks; modes: block/warn/pin/allow
- **Prometheus Metrics** — Pool, routing, cache, firewall, audit, digest, session, maintenance metrics
- **Admin API** — Bearer API Key auth with RBAC (admin/viewer), IP allowlist, trusted proxies
  - `/admin/stats`, `/admin/health`, `/admin/config`, `/admin/reload`
  - `/admin/cache/flush`, `/admin/cache/flush/{table}`
  - `/admin/queries/top`, `/admin/queries/reset`
  - `/admin/connections`, `/admin/maintenance`, `/admin/readonly`
  - `/admin/mirror/stats`
- **Health Check Endpoints** — `/healthz` (liveness), `/readyz` (readiness) without auth
- **Online Maintenance Mode** — Drain in-progress transactions, reject new connections
- **Read-Only Mode** — Block all write queries at proxy level
- **Serverless Data API** — `POST /v1/query` HTTP-to-SQL with JSON response, API Key auth
- **Audit Logging & Slow Query Tracker** — Structured audit logs, Webhook alerts with deduplication
- **Query Mirroring** — Async Shadow DB forwarding with P50/P99 latency comparison
- **Query Digest / Top-N Queries** — Normalized query pattern statistics (`pg_stat_statements` equivalent)
- **Query Timeout** — Proxy-level timeout with `CancelRequest`, per-query hint override
- **Idle Client Timeout** — Auto-disconnect idle clients with FATAL (57P01)
- **OpenTelemetry Distributed Tracing** — OTLP gRPC/stdout, `traceparent` context propagation
- **Config Hot-Reload** — fsnotify file watcher with ConfigMap symlink swap support
- **Circuit Breaker** — Closed/Open/Half-Open states for writer connection failure isolation
- **Token Bucket Rate Limiter** — Per-proxy query rate limiting
- **COPY Protocol** — Full COPY IN/OUT/BOTH relay support
- **Grafana Dashboard** — Pre-built dashboard template with Helm sidecar support
- **Kubernetes Helm Chart** — Multi-platform Docker image (amd64/arm64), HPA, PDB, ServiceMonitor
- **GitHub Actions CI/CD** — Lint, test (race), build, benchmark, Docker image auto-publish
- **SQL Redaction / Safe Observability** — `observability.sql_redaction` config (`none`/`literals`/`full`) to mask SQL literals in audit logs, OpenTelemetry spans, slog, and webhooks

### Fixed (Pre-release QA)

- CopyBoth goroutine race — drain both goroutines before returning (#247)
- Rate limiter clock skew — clamp negative elapsed time from NTP corrections (#247)
- SQL error message leak in Data API — return generic message instead (#247)
- Cache stale tableIndex on Set() update — remove old references before update (#247)
- MaxMessageSize guard in relay loop — prevent OOM on oversized backend messages (#248)
- Synthesizer memory exhaustion — LRU eviction at 10k statement limit (#248)
- Config watcher startup blocking — timeout with context cancellation (#248)
- `parseSize` silent zero on invalid input — log warning (#248)
- String/AST parser parity for EXPLAIN ANALYZE, MERGE, COPY, COMMENT (#249)
- ABORT as transaction end synonym (#249)
- Data API COPY statement rejection (#249)
- SET CONSTRAINTS false positive in session dependency detection (#249)
- Parser/router hint injection and bypass vulnerabilities (#246)
- Side-effectful SELECT classification (`nextval`, `setval`, `pg_advisory_lock`, etc.) (#238)
- Read-only mode enforcement on prepared statement reuse (#236)

[1.0.0]: https://github.com/jyukki97/pgmux/releases/tag/v1.0.0
