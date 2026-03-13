#!/usr/bin/env bash
set -euo pipefail

# pgmux Benchmark: Direct DB vs pgmux vs PgBouncer
#
# Prerequisites:
#   - Docker running
#   - pgbench (comes with PostgreSQL)
#   - pgmux binary built (make build)
#
# Usage:
#   make bench-compare
#   # or
#   ./scripts/bench-compare.sh

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Add PostgreSQL tools to PATH if installed via Homebrew
for pg_dir in /opt/homebrew/opt/postgresql@*/bin /usr/local/opt/postgresql@*/bin; do
    [ -d "$pg_dir" ] && export PATH="$pg_dir:$PATH"
done

# Ports
DIRECT_PORT=25432
PGMUX_PORT=35432
PGBOUNCER_PORT=26432

# Benchmark parameters
CLIENTS=${BENCH_CLIENTS:-"1 10 50 100"}
DURATION=${BENCH_DURATION:-10}
DB_USER="postgres"
DB_NAME="testdb"

RESULTS_DIR="$PROJECT_DIR/bench-results"
mkdir -p "$RESULTS_DIR"
RESULTS_FILE="$RESULTS_DIR/results.md"

cleanup() {
    echo "Cleaning up..."
    # Stop pgmux if running
    if [ -n "${PGMUX_PID:-}" ] && kill -0 "$PGMUX_PID" 2>/dev/null; then
        kill "$PGMUX_PID" 2>/dev/null || true
        wait "$PGMUX_PID" 2>/dev/null || true
    fi
    docker-compose -f "$PROJECT_DIR/docker-compose.bench.yml" down -v 2>/dev/null || true
}
trap cleanup EXIT

echo "=== pgmux Benchmark Suite ==="
echo ""

# 1. Check dependencies
command -v pgbench >/dev/null 2>&1 || { echo "ERROR: pgbench not found. Install PostgreSQL client tools."; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found."; exit 1; }

# 2. Build pgmux
echo "[1/5] Building pgmux..."
cd "$PROJECT_DIR"
make build 2>&1 | tail -1

# 3. Start Docker services
echo "[2/5] Starting PostgreSQL + PgBouncer..."
docker-compose -f docker-compose.bench.yml down -v 2>/dev/null || true
docker-compose -f docker-compose.bench.yml up -d
echo "Waiting for services..."
sleep 5

# Wait for PostgreSQL to be ready
for i in $(seq 1 30); do
    if PGPASSWORD=postgres psql -h 127.0.0.1 -p $DIRECT_PORT -U $DB_USER -d $DB_NAME -c "SELECT 1" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

# 4. Seed benchmark data using pgbench -i (standard schema)
echo "[3/5] Seeding benchmark data (pgbench -i, scale=10)..."
PGPASSWORD=postgres pgbench -h 127.0.0.1 -p $DIRECT_PORT -U $DB_USER -d $DB_NAME -i -s 10 --quiet

# 5. Start pgmux
echo "[4/5] Starting pgmux..."
"$PROJECT_DIR/bin/pgmux" -config "$PROJECT_DIR/config.bench.yaml" &
PGMUX_PID=$!
sleep 2

# Verify pgmux is running
if ! PGPASSWORD=postgres psql -h 127.0.0.1 -p $PGMUX_PORT -U $DB_USER -d $DB_NAME -c "SELECT 1" >/dev/null 2>&1; then
    echo "ERROR: pgmux failed to start"
    exit 1
fi

# 6. Run benchmarks
echo "[5/5] Running benchmarks (duration=${DURATION}s per test)..."
echo ""

# Write results header
cat > "$RESULTS_FILE" <<'HEADER'
# pgmux Benchmark Results

## Environment

HEADER

echo "- **Date**: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$RESULTS_FILE"
echo "- **OS**: $(uname -s) $(uname -m)" >> "$RESULTS_FILE"
echo "- **CPU**: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || nproc)" >> "$RESULTS_FILE"
echo "- **PostgreSQL**: $(PGPASSWORD=postgres psql -h 127.0.0.1 -p $DIRECT_PORT -U $DB_USER -d $DB_NAME -tAc 'SHOW server_version')" >> "$RESULTS_FILE"
echo "- **PgBouncer**: latest (transaction mode, pool_size=20)" >> "$RESULTS_FILE"
echo "- **pgmux**: pool min=5, max=20, cache=off, firewall=off" >> "$RESULTS_FILE"
echo "- **Data**: 100k accounts (pgbench-like schema)" >> "$RESULTS_FILE"
echo "" >> "$RESULTS_FILE"

run_pgbench() {
    local label="$1"
    local host="$2"
    local port="$3"
    local clients="$4"
    local mode="$5"  # "select" or "tpcb"

    local threads=$(( clients < 4 ? clients : 4 ))
    local pgbench_args="-h $host -p $port -U $DB_USER -d $DB_NAME -c $clients -j $threads -T $DURATION --no-vacuum"

    if [ "$mode" = "select" ]; then
        pgbench_args="$pgbench_args -S"
    fi

    local tmpfile
    tmpfile=$(mktemp)
    PGPASSWORD=postgres pgbench $pgbench_args > "$tmpfile" 2>/dev/null || true

    local tps latency
    tps=$(grep "tps = " "$tmpfile" | grep -oE '[0-9]+\.[0-9]+' | head -1)
    latency=$(grep "latency average" "$tmpfile" | grep -oE '[0-9]+\.[0-9]+')
    rm -f "$tmpfile"

    if [ -z "$tps" ]; then
        tps="error"
        latency="error"
    fi

    echo "$label|$clients|$tps|$latency"
}

# Select-only benchmark
echo "## SELECT-only (read workload)" >> "$RESULTS_FILE"
echo "" >> "$RESULTS_FILE"
echo "| Target | Clients | TPS | Avg Latency (ms) |" >> "$RESULTS_FILE"
echo "|--------|---------|-----|-------------------|" >> "$RESULTS_FILE"

for c in $CLIENTS; do
    echo "  SELECT-only: clients=$c"
    for target in "Direct|127.0.0.1|$DIRECT_PORT" "pgmux|127.0.0.1|$PGMUX_PORT" "PgBouncer|127.0.0.1|$PGBOUNCER_PORT"; do
        IFS='|' read -r label host port <<< "$target"
        result=$(run_pgbench "$label" "$host" "$port" "$c" "select")
        IFS='|' read -r _ _ tps latency <<< "$result"
        echo "| $label | $c | $tps | $latency |" >> "$RESULTS_FILE"
        printf "    %-10s c=%3s  TPS=%-10s Lat=%s ms\n" "$label" "$c" "$tps" "$latency"
    done
done

echo "" >> "$RESULTS_FILE"

# TPC-B (mixed read/write)
echo "## TPC-B (mixed read/write workload)" >> "$RESULTS_FILE"
echo "" >> "$RESULTS_FILE"
echo "| Target | Clients | TPS | Avg Latency (ms) |" >> "$RESULTS_FILE"
echo "|--------|---------|-----|-------------------|" >> "$RESULTS_FILE"

for c in $CLIENTS; do
    echo "  TPC-B: clients=$c"
    for target in "Direct|127.0.0.1|$DIRECT_PORT" "pgmux|127.0.0.1|$PGMUX_PORT" "PgBouncer|127.0.0.1|$PGBOUNCER_PORT"; do
        IFS='|' read -r label host port <<< "$target"
        result=$(run_pgbench "$label" "$host" "$port" "$c" "tpcb")
        IFS='|' read -r _ _ tps latency <<< "$result"
        echo "| $label | $c | $tps | $latency |" >> "$RESULTS_FILE"
        printf "    %-10s c=%3s  TPS=%-10s Lat=%s ms\n" "$label" "$c" "$tps" "$latency"
    done
done

echo "" >> "$RESULTS_FILE"
echo "---" >> "$RESULTS_FILE"
echo "" >> "$RESULTS_FILE"
echo "> Benchmarked with \`pgbench -T ${DURATION}\`. Lower latency and higher TPS is better." >> "$RESULTS_FILE"
echo "> Cache and firewall disabled for fair comparison (proxy overhead only)." >> "$RESULTS_FILE"

echo ""
echo "=== Benchmark complete ==="
echo "Results saved to: $RESULTS_FILE"
cat "$RESULTS_FILE"
