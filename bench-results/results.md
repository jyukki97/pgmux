# pgmux Benchmark Results

## Environment

- **Date**: 2026-03-13T07:12:10Z
- **OS**: Darwin arm64
- **CPU**: Apple M4 Pro
- **PostgreSQL**: 16.13
- **PgBouncer**: latest (transaction mode, pool_size=20)
- **pgmux**: pool min=5, max=20, cache=off, firewall=off
- **Data**: 100k accounts (pgbench-like schema)

## SELECT-only (read workload)

| Target | Clients | TPS | Avg Latency (ms) |
|--------|---------|-----|-------------------|
| Direct | 10 | 16932.896822 | 0.591 |
| pgmux | 10 | 14395.937219 | 0.695 |
| PgBouncer | 10 | 14983.646808 | 0.667 |
| Direct | 50 | 25533.290695 | 1.958 |
| pgmux | 50 | 21130.507930 | 2.366 |
| PgBouncer | 50 | 24826.683153 | 2.014 |

## TPC-B (mixed read/write workload)

| Target | Clients | TPS | Avg Latency (ms) |
|--------|---------|-----|-------------------|
| Direct | 10 | 2364.930085 | 4.228 |
| pgmux | 10 | 1830.527939 | 5.463 |
| PgBouncer | 10 | 2073.817422 | 4.822 |
| Direct | 50 | 3274.755549 | 15.268 |
| pgmux | 50 | 2368.675129 | 21.109 |
| PgBouncer | 50 | 2716.979381 | 18.403 |

---

> Benchmarked with `pgbench -T 15`. Lower latency and higher TPS is better.
> Cache and firewall disabled for fair comparison (proxy overhead only).
