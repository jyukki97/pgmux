# pgmux Benchmark Results

## Environment

- **Date**: 2026-03-13T14:43:41Z
- **OS**: Darwin arm64
- **CPU**: Apple M4 Pro
- **PostgreSQL**: 16.13
- **PgBouncer**: latest (transaction mode, pool_size=20)
- **pgmux**: pool min=5, max=20, cache=off, firewall=off
- **Data**: 100k accounts (pgbench-like schema)
- **Methodology**: warmup 5s + 3-round average (10s each)

## SELECT-only (read workload)

| Target | Clients | TPS | Avg Latency (ms) |
|--------|---------|-----|-------------------|
| Direct | 1 | 2932.90 | .34 |
| pgmux | 1 | 2565.60 | .39 |
| PgBouncer | 1 | 2193.27 | .45 |
| Direct | 10 | 16551.54 | .60 |
| pgmux | 10 | 14335.70 | .69 |
| PgBouncer | 10 | 15127.74 | .66 |
| Direct | 50 | 26008.37 | 1.92 |
| pgmux | 50 | 19904.61 | 2.52 |
| PgBouncer | 50 | 23264.24 | 2.15 |
| Direct | 100 | 24157.74 | 4.14 |
| pgmux | 100 | 17696.42 | 5.66 |
| PgBouncer | 100 | 23838.49 | 4.19 |

## TPC-B (mixed read/write workload)

| Target | Clients | TPS | Avg Latency (ms) |
|--------|---------|-----|-------------------|
| Direct | 1 | 383.32 | 2.61 |
| pgmux | 1 | 374.10 | 2.67 |
| PgBouncer | 1 | 351.31 | 2.84 |
| Direct | 10 | 2218.39 | 4.52 |
| pgmux | 10 | 1869.78 | 5.35 |
| PgBouncer | 10 | 2067.59 | 4.83 |
| Direct | 50 | 3086.80 | 16.20 |
| pgmux | 50 | 2386.54 | 21.05 |
| PgBouncer | 50 | 2473.65 | 20.22 |
| Direct | 100 | 2823.40 | 35.43 |
| pgmux | 100 | 2393.19 | 41.87 |
| PgBouncer | 100 | 2433.92 | 41.28 |

---

> Benchmarked with `pgbench -T 10`, 3-round average with 5s warmup. Lower latency and higher TPS is better.
> Cache and firewall disabled for fair comparison (proxy overhead only).
