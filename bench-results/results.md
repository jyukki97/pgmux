# pgmux Benchmark Results

## Environment

- **Date**: 2026-03-13
- **OS**: macOS (Apple M4 Pro, arm64)
- **PostgreSQL**: 16.13 (Docker)
- **PgBouncer**: 1.25.1 (transaction mode, pool_size=20)
- **pgmux**: pool min=5, max=20, cache=off, firewall=off
- **Data**: pgbench scale=10 (1M rows)
- **Tool**: pgbench -T 10

## SELECT-only (read workload)

| Target | Clients | TPS | Avg Latency (ms) | vs Direct |
|--------|---------|-----|-------------------|-----------|
| Direct | 1 | 2,999 | 0.33 | - |
| pgmux | 1 | 1,512 | 0.66 | 50% |
| PgBouncer | 1 | 2,527 | 0.40 | 84% |
| Direct | 10 | 16,883 | 0.59 | - |
| pgmux | 10 | 8,589 | 1.16 | 51% |
| PgBouncer | 10 | 14,886 | 0.67 | 88% |
| Direct | 50 | 25,806 | 1.94 | - |
| pgmux | 50 | 11,879 | 4.21 | 46% |
| PgBouncer | 50 | 25,354 | 1.97 | 98% |

## TPC-B (mixed read/write workload)

| Target | Clients | TPS | Avg Latency (ms) | vs Direct |
|--------|---------|-----|-------------------|-----------|
| Direct | 1 | 435 | 2.30 | - |
| pgmux | 1 | 333 | 3.00 | 77% |
| PgBouncer | 1 | 367 | 2.72 | 84% |
| Direct | 10 | 2,331 | 4.29 | - |
| pgmux | 10 | 1,820 | 5.49 | 78% |
| PgBouncer | 10 | 2,028 | 4.93 | 87% |
| Direct | 50 | 3,227 | 15.50 | - |
| pgmux | 50 | 2,345 | 21.32 | 73% |
| PgBouncer | 50 | 2,707 | 18.47 | 84% |

## Analysis

- **TPC-B (I/O bound)**: pgmux achieves 73-78% of direct connection throughput, comparable to PgBouncer (84%). The gap narrows because DB I/O dominates proxy overhead.
- **SELECT-only (CPU bound)**: PgBouncer (C) has lower proxy overhead than pgmux (Go). This is expected for lightweight queries where proxy processing time is a larger fraction of total latency.
- **Trade-off**: pgmux provides query caching, firewall, audit logging, query mirroring, and multi-database routing that PgBouncer lacks. With caching enabled, pgmux can exceed direct connection performance for repeated queries.

## Reproduce

```bash
make bench-compare
# or with custom parameters:
BENCH_CLIENTS="1 10 50 100" BENCH_DURATION=30 make bench-compare
```
