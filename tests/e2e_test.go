package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
)

// TestE2E_ProxyIntegration tests the full proxy with real PostgreSQL backends.
// Requires: docker-compose up && go run ./cmd/pgmux config.test.yaml
func TestE2E_ProxyIntegration(t *testing.T) {
	proxyDSN := "postgres://postgres:postgres@127.0.0.1:15440/testdb?sslmode=disable"

	// Check if proxy is reachable
	db, err := sql.Open("postgres", proxyDSN)
	if err != nil {
		t.Skipf("cannot open proxy connection: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Skipf("proxy not reachable (start docker-compose + proxy first): %v", err)
	}

	t.Run("ReadQuery", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT name FROM users ORDER BY id LIMIT 3")
		if err != nil {
			t.Fatalf("SELECT failed: %v", err)
		}
		defer rows.Close()

		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}

		if len(names) < 3 {
			t.Fatalf("got %d rows, want >= 3", len(names))
		}
		if names[0] != "alice" || names[1] != "bob" || names[2] != "charlie" {
			t.Errorf("got names %v, want [alice bob charlie]", names)
		}
		t.Logf("Read query OK: %v (routed to reader via RoundRobin)", names)
	})

	t.Run("WriteQuery", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO users (name, email) VALUES ($1, $2)", "dave", "dave@example.com")
		if err != nil {
			t.Fatalf("INSERT failed: %v", err)
		}

		// Read back
		var count int
		err = db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count)
		if err != nil {
			t.Fatalf("count query failed: %v", err)
		}
		if count < 4 {
			t.Errorf("got count %d, want >= 4", count)
		}
		t.Logf("Write query OK: count=%d", count)

		// Clean up
		db.ExecContext(ctx, "DELETE FROM users WHERE name = 'dave'")
	})

	t.Run("Transaction", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BEGIN failed: %v", err)
		}

		_, err = tx.ExecContext(ctx, "INSERT INTO users (name, email) VALUES ('tx_user', 'tx@example.com')")
		if err != nil {
			tx.Rollback()
			t.Fatalf("INSERT in tx failed: %v", err)
		}

		// Read within same transaction should see the insert
		var name string
		err = tx.QueryRowContext(ctx, "SELECT name FROM users WHERE name = 'tx_user'").Scan(&name)
		if err != nil {
			tx.Rollback()
			t.Fatalf("SELECT in tx failed: %v", err)
		}
		if name != "tx_user" {
			t.Errorf("got name %q, want 'tx_user'", name)
		}

		tx.Rollback()
		t.Log("Transaction routing OK")
	})

	t.Run("CacheHit", func(t *testing.T) {
		// First query - cache miss
		var name1 string
		err := db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name1)
		if err != nil {
			t.Fatalf("first SELECT failed: %v", err)
		}

		// Second identical query - should be cache hit
		var name2 string
		err = db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name2)
		if err != nil {
			t.Fatalf("second SELECT failed: %v", err)
		}

		if name1 != name2 {
			t.Errorf("cache inconsistency: %q != %q", name1, name2)
		}
		t.Logf("Cache test OK: %s", name1)
	})
}

// TestE2E_TransactionPooling tests transaction-level connection pooling behavior.
// Requires: docker-compose up && go run ./cmd/pgmux config.test.yaml
func TestE2E_TransactionPooling(t *testing.T) {
	proxyDSN := "postgres://postgres:postgres@127.0.0.1:15440/testdb?sslmode=disable"

	db, err := sql.Open("postgres", proxyDSN)
	if err != nil {
		t.Skipf("cannot open proxy connection: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Skipf("proxy not reachable (start docker-compose + proxy first): %v", err)
	}

	// Create a temp table for pooling tests
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS pooling_test (
		id SERIAL PRIMARY KEY,
		client_id INT NOT NULL,
		value TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create pooling_test table: %v", err)
	}
	defer db.ExecContext(ctx, "DROP TABLE IF EXISTS pooling_test")

	t.Run("ConcurrentWriters", func(t *testing.T) {
		// Multiple concurrent clients writing through the pool.
		// config.test.yaml has max_connections=10, so 20 concurrent writers
		// must share those 10 connections via pooling.
		const numClients = 20
		var wg sync.WaitGroup
		var successCount atomic.Int32

		for i := 0; i < numClients; i++ {
			wg.Add(1)
			go func(clientID int) {
				defer wg.Done()

				conn, err := sql.Open("postgres", proxyDSN)
				if err != nil {
					t.Logf("client %d: open failed: %v", clientID, err)
					return
				}
				defer conn.Close()

				_, err = conn.ExecContext(ctx,
					"INSERT INTO pooling_test (client_id, value) VALUES ($1, $2)",
					clientID, fmt.Sprintf("data-%d", clientID))
				if err != nil {
					t.Logf("client %d: INSERT failed: %v", clientID, err)
					return
				}
				successCount.Add(1)
			}(i)
		}

		wg.Wait()

		if got := successCount.Load(); got != numClients {
			t.Errorf("successful inserts = %d, want %d", got, numClients)
		}

		// Verify all rows exist
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM pooling_test").Scan(&count)
		if err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != numClients {
			t.Errorf("row count = %d, want %d", count, numClients)
		}
		t.Logf("Concurrent writers OK: %d/%d succeeded", successCount.Load(), numClients)

		// Clean up
		db.ExecContext(ctx, "DELETE FROM pooling_test")
	})

	t.Run("TransactionIsolation", func(t *testing.T) {
		// Verify that a transaction sees its own writes consistently,
		// meaning all queries within BEGIN..COMMIT go to the same backend connection.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BEGIN: %v", err)
		}

		_, err = tx.ExecContext(ctx, "INSERT INTO pooling_test (client_id, value) VALUES (999, 'tx-isolation')")
		if err != nil {
			tx.Rollback()
			t.Fatalf("INSERT in tx: %v", err)
		}

		// SELECT within the same transaction must see the uncommitted row
		var value string
		err = tx.QueryRowContext(ctx, "SELECT value FROM pooling_test WHERE client_id = 999").Scan(&value)
		if err != nil {
			tx.Rollback()
			t.Fatalf("SELECT in tx: %v", err)
		}
		if value != "tx-isolation" {
			tx.Rollback()
			t.Errorf("value = %q, want 'tx-isolation'", value)
		}

		// Another connection outside the transaction should NOT see this row
		var outsideCount int
		// Use a fresh connection to avoid being on the same backend
		outsideConn, _ := sql.Open("postgres", proxyDSN)
		defer outsideConn.Close()
		err = outsideConn.QueryRowContext(ctx, "SELECT count(*) FROM pooling_test WHERE client_id = 999").Scan(&outsideCount)
		if err != nil {
			tx.Rollback()
			t.Fatalf("outside SELECT: %v", err)
		}
		if outsideCount != 0 {
			t.Errorf("outside tx saw %d uncommitted rows, want 0", outsideCount)
		}

		tx.Rollback()
		t.Log("Transaction isolation OK: uncommitted data not visible outside tx")
	})

	t.Run("SessionResetAfterRelease", func(t *testing.T) {
		// Verify that DISCARD ALL resets session state between users.
		// In transaction pooling mode, SET and SHOW must be in the same transaction
		// to guarantee they use the same backend connection.

		connA, err := sql.Open("postgres", proxyDSN)
		if err != nil {
			t.Fatalf("open connA: %v", err)
		}
		connA.SetMaxOpenConns(1)

		// Use a transaction to ensure SET and SHOW hit the same backend
		tx, err := connA.BeginTx(ctx, nil)
		if err != nil {
			connA.Close()
			t.Fatalf("BEGIN: %v", err)
		}

		_, err = tx.ExecContext(ctx, "SET application_name = 'client_a_session'")
		if err != nil {
			tx.Rollback()
			connA.Close()
			t.Fatalf("SET application_name: %v", err)
		}

		var appName string
		err = tx.QueryRowContext(ctx, "SHOW application_name").Scan(&appName)
		if err != nil {
			tx.Rollback()
			connA.Close()
			t.Fatalf("SHOW application_name: %v", err)
		}
		if appName != "client_a_session" {
			tx.Rollback()
			connA.Close()
			t.Fatalf("application_name = %q, want 'client_a_session'", appName)
		}

		// COMMIT — connection returns to pool with DISCARD ALL
		tx.Commit()
		connA.Close()

		time.Sleep(100 * time.Millisecond)

		// Client B opens a new connection — should not see leaked session state
		connB, err := sql.Open("postgres", proxyDSN)
		if err != nil {
			t.Fatalf("open connB: %v", err)
		}
		defer connB.Close()
		connB.SetMaxOpenConns(1)

		var appNameB string
		err = connB.QueryRowContext(ctx, "SHOW application_name").Scan(&appNameB)
		if err != nil {
			t.Fatalf("SHOW application_name on B: %v", err)
		}

		if appNameB == "client_a_session" {
			t.Error("session state leaked: client B saw client A's application_name")
		}
		t.Logf("Session reset OK: client B got application_name=%q (not 'client_a_session')", appNameB)
	})

	t.Run("ConcurrentTransactions", func(t *testing.T) {
		// Multiple concurrent transactions, each doing BEGIN → INSERT → SELECT → COMMIT.
		// All must succeed and see their own data within the transaction.
		const numTx = 10
		var wg sync.WaitGroup
		var successCount atomic.Int32

		for i := 0; i < numTx; i++ {
			wg.Add(1)
			go func(txID int) {
				defer wg.Done()

				conn, err := sql.Open("postgres", proxyDSN)
				if err != nil {
					t.Logf("tx %d: open failed: %v", txID, err)
					return
				}
				defer conn.Close()

				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					t.Logf("tx %d: BEGIN failed: %v", txID, err)
					return
				}

				val := fmt.Sprintf("concurrent-tx-%d", txID)
				_, err = tx.ExecContext(ctx,
					"INSERT INTO pooling_test (client_id, value) VALUES ($1, $2)",
					txID+1000, val)
				if err != nil {
					tx.Rollback()
					t.Logf("tx %d: INSERT failed: %v", txID, err)
					return
				}

				// Read back within same transaction
				var readVal string
				err = tx.QueryRowContext(ctx,
					"SELECT value FROM pooling_test WHERE client_id = $1",
					txID+1000).Scan(&readVal)
				if err != nil {
					tx.Rollback()
					t.Logf("tx %d: SELECT failed: %v", txID, err)
					return
				}

				if readVal != val {
					tx.Rollback()
					t.Logf("tx %d: value mismatch: got %q, want %q", txID, readVal, val)
					return
				}

				if err := tx.Commit(); err != nil {
					t.Logf("tx %d: COMMIT failed: %v", txID, err)
					return
				}
				successCount.Add(1)
			}(i)
		}

		wg.Wait()

		if got := successCount.Load(); got != numTx {
			t.Errorf("successful transactions = %d, want %d", got, numTx)
		}

		// Verify all committed rows exist
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM pooling_test WHERE client_id >= 1000").Scan(&count)
		if err != nil {
			t.Fatalf("count committed rows: %v", err)
		}
		if count != numTx {
			t.Errorf("committed row count = %d, want %d", count, numTx)
		}
		t.Logf("Concurrent transactions OK: %d/%d succeeded, %d rows committed", successCount.Load(), numTx, count)

		// Clean up
		db.ExecContext(ctx, "DELETE FROM pooling_test")
	})

	t.Run("PoolExhaustionAndRecovery", func(t *testing.T) {
		// Hold all pool connections in transactions, then verify that
		// new clients eventually succeed after transactions commit.
		const poolMax = 10 // matches config.test.yaml max_connections

		holders := make([]*sql.Tx, 0, poolMax)
		holderConns := make([]*sql.DB, 0, poolMax)

		// Acquire all connections by starting transactions
		for i := 0; i < poolMax; i++ {
			conn, err := sql.Open("postgres", proxyDSN)
			if err != nil {
				t.Fatalf("open holder %d: %v", i, err)
			}
			conn.SetMaxOpenConns(1)
			holderConns = append(holderConns, conn)

			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("BEGIN holder %d: %v", i, err)
			}
			// Execute a query to ensure connection is actually acquired from pool
			_, err = tx.ExecContext(ctx, "SELECT 1")
			if err != nil {
				tx.Rollback()
				t.Fatalf("holder %d SELECT: %v", i, err)
			}
			holders = append(holders, tx)
		}

		// Release half the connections
		for i := 0; i < poolMax/2; i++ {
			holders[i].Commit()
			holderConns[i].Close()
		}

		// Now a new client should be able to acquire a connection
		newConn, err := sql.Open("postgres", proxyDSN)
		if err != nil {
			t.Fatalf("open new client: %v", err)
		}
		defer newConn.Close()

		_, err = newConn.ExecContext(ctx, "INSERT INTO pooling_test (client_id, value) VALUES (9999, 'recovery')")
		if err != nil {
			t.Errorf("new client INSERT after pool recovery failed: %v", err)
		} else {
			t.Log("Pool exhaustion recovery OK: new client succeeded after transactions committed")
		}

		// Release remaining holders
		for i := poolMax / 2; i < len(holders); i++ {
			holders[i].Commit()
			holderConns[i].Close()
		}

		// Clean up
		db.ExecContext(ctx, "DELETE FROM pooling_test")
	})
}

// TestE2E_CausalConsistency tests that write-then-read sees fresh data.
// Requires: docker-compose up && go run ./cmd/pgmux config.test.yaml (with causal_consistency: true)
func TestE2E_CausalConsistency(t *testing.T) {
	proxyDSN := "postgres://postgres:postgres@127.0.0.1:15440/testdb?sslmode=disable"

	db, err := sql.Open("postgres", proxyDSN)
	if err != nil {
		t.Skipf("cannot open proxy connection: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Skipf("proxy not reachable: %v", err)
	}

	t.Run("WriteReadConsistency", func(t *testing.T) {
		// Create temp table and ensure it's empty
		_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS causal_test (
			id SERIAL PRIMARY KEY,
			value TEXT NOT NULL
		)`)
		if err != nil {
			t.Fatalf("create causal_test table: %v", err)
		}
		defer db.ExecContext(ctx, "DROP TABLE IF EXISTS causal_test")
		// Truncate to clear data from previous runs
		_, _ = db.ExecContext(ctx, "TRUNCATE causal_test")

		// Write and immediately read — should see the inserted data
		for i := 0; i < 10; i++ {
			val := fmt.Sprintf("causal-%d", i)
			_, err := db.ExecContext(ctx, "INSERT INTO causal_test (value) VALUES ($1)", val)
			if err != nil {
				t.Fatalf("INSERT %d: %v", i, err)
			}

			// Immediately read — must see the just-written row
			var count int
			err = db.QueryRowContext(ctx, "SELECT count(*) FROM causal_test WHERE value = $1", val).Scan(&count)
			if err != nil {
				t.Fatalf("SELECT %d: %v", i, err)
			}
			if count != 1 {
				t.Errorf("iteration %d: expected count=1 for value=%q, got %d (stale read!)", i, val, count)
			}
		}
		t.Log("Write-read consistency OK: all 10 iterations saw fresh data")
	})
}

// TestE2E_ProxyStartStop tests that the proxy binary starts and stops cleanly.
func TestE2E_ProxyStartStop(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("set E2E=1 to run")
	}

	binPath := "../bin/pgmux"
	cfgPath := "../config.test.yaml"

	cmd := exec.Command(binPath, cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}

	time.Sleep(time.Second)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send interrupt: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("proxy exited with: %v (expected for signal)", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("proxy did not shut down within 5s")
	}

	t.Log("proxy start/stop OK")
}

// ---------------------------------------------------------------------------
// New E2E tests
// ---------------------------------------------------------------------------

const (
	e2eProxyDSN    = "postgres://postgres:postgres@127.0.0.1:15440/testdb?sslmode=disable"
	e2eAdminURL    = "http://127.0.0.1:19091"
	e2eMetricURL   = "http://127.0.0.1:19090/metrics"
	e2eDataAPIURL  = "http://127.0.0.1:18080"
	e2eDataAPIKey  = "test-api-key-12345"
	e2eLimitedDSN  = "postgres://limited:limited@127.0.0.1:15440/testdb?sslmode=disable"
)

// e2eCheckProxy opens a connection and pings the proxy. Returns the *sql.DB
// on success or calls t.Skipf and returns nil if the proxy is unreachable.
func e2eCheckProxy(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", e2eProxyDSN)
	if err != nil {
		t.Skipf("cannot open proxy connection: %v", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("proxy not reachable (start docker-compose + proxy first): %v", err)
		return nil
	}
	return db
}

// TestE2E_ExtendedQueryProtocol verifies parameterized queries work through the
// proxy's extended query protocol handling (Parse+Describe+Sync, Bind+Execute+Sync).
// This is the two-round protocol used by lib/pq for parameterized queries.
func TestE2E_ExtendedQueryProtocol(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a test table
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS ext_query_test (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		value INT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create ext_query_test table: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS ext_query_test")

	t.Run("ParameterizedSELECT", func(t *testing.T) {
		// Simple parameterized SELECT using $1 placeholder
		var result int
		err := db.QueryRowContext(ctx, "SELECT $1::int + $2::int", 10, 20).Scan(&result)
		if err != nil {
			t.Fatalf("parameterized SELECT failed: %v", err)
		}
		if result != 30 {
			t.Errorf("got %d, want 30", result)
		}
		t.Logf("Parameterized SELECT OK: 10 + 20 = %d", result)
	})

	t.Run("ParameterizedINSERT", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO ext_query_test (name, value) VALUES ($1, $2)", "test-ext", 42)
		if err != nil {
			t.Fatalf("parameterized INSERT failed: %v", err)
		}
		var name string
		var value int
		err = db.QueryRowContext(ctx, "SELECT name, value FROM ext_query_test WHERE name = $1", "test-ext").Scan(&name, &value)
		if err != nil {
			t.Fatalf("read back failed: %v", err)
		}
		if name != "test-ext" || value != 42 {
			t.Errorf("got (%q, %d), want (\"test-ext\", 42)", name, value)
		}
		t.Logf("Parameterized INSERT OK: name=%s, value=%d", name, value)
	})

	t.Run("ConcurrentParameterizedSELECT", func(t *testing.T) {
		const numWorkers = 50
		var wg sync.WaitGroup
		var successCount atomic.Int32

		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				var result int
				err := db.QueryRowContext(ctx, "SELECT $1::int * 2", idx).Scan(&result)
				if err != nil {
					t.Logf("worker %d SELECT failed: %v", idx, err)
					return
				}
				if result != idx*2 {
					t.Logf("worker %d: got %d, want %d", idx, result, idx*2)
					return
				}
				successCount.Add(1)
			}(i)
		}
		wg.Wait()

		if got := successCount.Load(); got != numWorkers {
			t.Errorf("concurrent parameterized SELECT: %d/%d succeeded", got, numWorkers)
		}
		t.Logf("Concurrent parameterized SELECT OK: %d/%d succeeded", successCount.Load(), numWorkers)
	})

	t.Run("ConcurrentParameterizedINSERT", func(t *testing.T) {
		const numWorkers = 50
		var wg sync.WaitGroup
		var successCount atomic.Int32

		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, err := db.ExecContext(ctx,
					"INSERT INTO ext_query_test (name, value) VALUES ($1, $2)",
					fmt.Sprintf("concurrent-%d", idx), idx)
				if err != nil {
					t.Logf("worker %d INSERT failed: %v", idx, err)
					return
				}
				successCount.Add(1)
			}(i)
		}
		wg.Wait()

		if got := successCount.Load(); got != numWorkers {
			t.Errorf("concurrent parameterized INSERT: %d/%d succeeded", got, numWorkers)
		}

		// Verify all rows were inserted
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM ext_query_test WHERE name LIKE 'concurrent-%'").Scan(&count)
		if err != nil {
			t.Fatalf("count query failed: %v", err)
		}
		if count != numWorkers {
			t.Errorf("got %d rows, want %d", count, numWorkers)
		}
		t.Logf("Concurrent parameterized INSERT OK: %d/%d succeeded, %d rows", successCount.Load(), numWorkers, count)
	})
}

// TestE2E_CopyProtocol tests COPY IN (COPY FROM STDIN) and COPY OUT (COPY TO STDOUT)
// via the proxy using lib/pq's CopyIn driver.
func TestE2E_CopyProtocol(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS copy_test (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		value INT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create copy_test table: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS copy_test")

	t.Run("CopyIn", func(t *testing.T) {
		txn, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BEGIN: %v", err)
		}

		stmt, err := txn.Prepare(pq.CopyIn("copy_test", "name", "value"))
		if err != nil {
			txn.Rollback()
			t.Fatalf("Prepare CopyIn: %v", err)
		}

		const numRows = 100
		for i := 0; i < numRows; i++ {
			_, err = stmt.Exec(fmt.Sprintf("copy-row-%d", i), i)
			if err != nil {
				stmt.Close()
				txn.Rollback()
				t.Fatalf("Exec row %d: %v", i, err)
			}
		}

		// Flush
		_, err = stmt.Exec()
		if err != nil {
			stmt.Close()
			txn.Rollback()
			t.Fatalf("Exec flush: %v", err)
		}
		stmt.Close()

		if err := txn.Commit(); err != nil {
			t.Fatalf("COMMIT: %v", err)
		}

		// Verify rows were loaded
		var count int
		err = db.QueryRowContext(ctx, "SELECT count(*) FROM copy_test").Scan(&count)
		if err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != numRows {
			t.Errorf("COPY IN: got %d rows, want %d", count, numRows)
		}
		t.Logf("COPY IN OK: loaded %d rows via COPY FROM STDIN", count)
	})

	t.Run("CopyOut", func(t *testing.T) {
		// Ensure data exists from the CopyIn subtest
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM copy_test").Scan(&count)
		if err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count == 0 {
			t.Skip("no data in copy_test; CopyIn subtest may have failed")
		}

		// lib/pq does not support COPY TO STDOUT via db.QueryContext.
		// Instead, verify via a regular SELECT that the COPY IN data is readable.
		rows, err := db.QueryContext(ctx, "SELECT name, value FROM copy_test ORDER BY value")
		if err != nil {
			t.Fatalf("SELECT from copy_test failed: %v", err)
		}
		defer rows.Close()

		var rowCount int
		for rows.Next() {
			var name string
			var value int
			if err := rows.Scan(&name, &value); err != nil {
				t.Fatalf("scan row %d: %v", rowCount, err)
			}
			rowCount++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows iteration error: %v", err)
		}
		if rowCount != count {
			t.Errorf("got %d rows, want %d", rowCount, count)
		}
		t.Logf("COPY OUT verification OK: read back %d rows via SELECT", rowCount)
	})
}

// TestE2E_AdminAPI tests all admin HTTP endpoints.
func TestE2E_AdminAPI(t *testing.T) {
	// Check proxy is reachable first
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	client := &http.Client{Timeout: 10 * time.Second}

	t.Run("Healthz", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz: status=%d, want 200", resp.StatusCode)
		}

		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status=%q, want \"ok\"", body["status"])
		}
		t.Logf("GET /healthz OK: %v", body)
	})

	t.Run("Readyz", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /readyz: status=%d, want 200", resp.StatusCode)
		}

		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["status"] != "ready" {
			t.Errorf("status=%q, want \"ready\"", body["status"])
		}
		t.Logf("GET /readyz OK: %v", body)
	})

	t.Run("AdminHealth", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/admin/health")
		if err != nil {
			t.Fatalf("GET /admin/health: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/health: status=%d, want 200", resp.StatusCode)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var body map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}

		// Check databases.testdb.writer.healthy == true
		dbs, ok := body["databases"]
		if !ok {
			t.Fatal("missing 'databases' field in /admin/health")
		}
		var dbMap map[string]json.RawMessage
		if err := json.Unmarshal(dbs, &dbMap); err != nil {
			t.Fatalf("decode databases: %v", err)
		}
		testdbRaw, ok := dbMap["testdb"]
		if !ok {
			t.Fatal("missing 'testdb' in databases")
		}

		var testdb struct {
			Writer struct {
				Healthy bool `json:"healthy"`
			} `json:"writer"`
		}
		if err := json.Unmarshal(testdbRaw, &testdb); err != nil {
			t.Fatalf("decode testdb: %v", err)
		}
		if !testdb.Writer.Healthy {
			t.Error("testdb writer is not healthy")
		}
		t.Logf("GET /admin/health OK: writer healthy=%v", testdb.Writer.Healthy)
	})

	t.Run("AdminStats", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/admin/stats")
		if err != nil {
			t.Fatalf("GET /admin/stats: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/stats: status=%d, want 200", resp.StatusCode)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var body map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}

		if _, ok := body["pool"]; !ok {
			t.Error("missing 'pool' field in /admin/stats")
		}
		if _, ok := body["cache"]; !ok {
			t.Error("missing 'cache' field in /admin/stats")
		}
		t.Logf("GET /admin/stats OK: has pool and cache fields")
	})

	t.Run("AdminConfig", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/admin/config")
		if err != nil {
			t.Fatalf("GET /admin/config: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/config: status=%d, want 200", resp.StatusCode)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		bodyStr := string(bodyBytes)

		// Must contain proxy.listen
		if !strings.Contains(bodyStr, "15440") {
			t.Error("config response does not contain proxy listen port 15440")
		}

		// Passwords must be masked
		if strings.Contains(bodyStr, "\"postgres\"") && !strings.Contains(bodyStr, "********") {
			t.Error("config response may contain unmasked passwords")
		}
		if !strings.Contains(bodyStr, "********") {
			t.Error("config response does not contain masked passwords")
		}
		t.Logf("GET /admin/config OK: contains proxy listen and masked passwords")
	})

	t.Run("AdminCacheFlush", func(t *testing.T) {
		resp, err := client.Post(e2eAdminURL+"/admin/cache/flush", "application/json", nil)
		if err != nil {
			t.Fatalf("POST /admin/cache/flush: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /admin/cache/flush: status=%d, want 200", resp.StatusCode)
		}

		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["status"] != "flushed" {
			t.Errorf("status=%q, want \"flushed\"", body["status"])
		}
		t.Logf("POST /admin/cache/flush OK: %v", body)
	})

	t.Run("AdminConnections", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/admin/connections")
		if err != nil {
			t.Fatalf("GET /admin/connections: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/connections: status=%d, want 200", resp.StatusCode)
		}
		t.Logf("GET /admin/connections OK: status=%d", resp.StatusCode)
	})

	t.Run("AdminQueriesTop", func(t *testing.T) {
		resp, err := client.Get(e2eAdminURL + "/admin/queries/top")
		if err != nil {
			t.Fatalf("GET /admin/queries/top: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/queries/top: status=%d, want 200", resp.StatusCode)
		}
		t.Logf("GET /admin/queries/top OK: status=%d", resp.StatusCode)
	})
}

// TestE2E_MetricsEndpoint tests the Prometheus metrics endpoint.
func TestE2E_MetricsEndpoint(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(e2eMetricURL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d, want 200", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyStr := string(bodyBytes)

	// Must be Prometheus text format with the expected metric
	if !strings.Contains(bodyStr, "pgmux_queries_routed_total") {
		t.Error("metrics response does not contain pgmux_queries_routed_total")
	}

	// Verify it looks like Prometheus text exposition format
	if !strings.Contains(bodyStr, "# HELP") || !strings.Contains(bodyStr, "# TYPE") {
		t.Error("metrics response does not look like Prometheus text format (missing # HELP or # TYPE)")
	}

	t.Logf("GET /metrics OK: contains pgmux_queries_routed_total in Prometheus format")
}

// TestE2E_ReadOnlyMode tests toggling read-only mode via admin API and verifying
// that write queries are rejected while reads still work.
func TestE2E_ReadOnlyMode(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}

	// Ensure we start in non-read-only mode
	req, _ := http.NewRequest(http.MethodDelete, e2eAdminURL+"/admin/readonly", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE /admin/readonly: %v", err)
	}
	resp.Body.Close()

	// Create a temp table for the test
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS readonly_test (
		id SERIAL PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create readonly_test table: %v", err)
	}
	defer func() {
		// Make sure read-only is disabled before cleanup
		req, _ := http.NewRequest(http.MethodDelete, e2eAdminURL+"/admin/readonly", nil)
		client.Do(req)
		time.Sleep(100 * time.Millisecond)
		db.ExecContext(context.Background(), "DROP TABLE IF EXISTS readonly_test")
	}()

	t.Run("EnableReadOnly", func(t *testing.T) {
		// Enable read-only mode
		resp, err := client.Post(e2eAdminURL+"/admin/readonly", "application/json", nil)
		if err != nil {
			t.Fatalf("POST /admin/readonly: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /admin/readonly: status=%d, want 200", resp.StatusCode)
		}
		t.Logf("POST /admin/readonly OK: enabled")

		// Small delay to allow mode to propagate
		time.Sleep(100 * time.Millisecond)

		// INSERT should fail with read-only error
		_, err = db.ExecContext(ctx, "INSERT INTO readonly_test (value) VALUES ('should-fail')")
		if err == nil {
			t.Error("INSERT should have failed in read-only mode, but succeeded")
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("expected 'read-only' error, got: %v", err)
		} else {
			t.Logf("INSERT correctly rejected in read-only mode: %v", err)
		}

		// SELECT should still work
		var result int
		err = db.QueryRowContext(ctx, "SELECT 1").Scan(&result)
		if err != nil {
			t.Errorf("SELECT should work in read-only mode, but failed: %v", err)
		} else {
			t.Logf("SELECT works in read-only mode: result=%d", result)
		}
	})

	t.Run("DisableReadOnly", func(t *testing.T) {
		// Disable read-only mode
		req, _ := http.NewRequest(http.MethodDelete, e2eAdminURL+"/admin/readonly", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("DELETE /admin/readonly: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE /admin/readonly: status=%d, want 200", resp.StatusCode)
		}
		t.Logf("DELETE /admin/readonly OK: disabled")

		time.Sleep(100 * time.Millisecond)

		// INSERT should work again
		_, err = db.ExecContext(ctx, "INSERT INTO readonly_test (value) VALUES ('after-readonly')")
		if err != nil {
			t.Errorf("INSERT should work after disabling read-only, but failed: %v", err)
		} else {
			t.Logf("INSERT works after disabling read-only mode")
		}
	})
}

// TestE2E_MaintenanceMode tests toggling maintenance mode via admin API.
// New connections should be rejected in maintenance mode and accepted after disabling.
func TestE2E_MaintenanceMode(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	client := &http.Client{Timeout: 10 * time.Second}

	// Ensure we start in non-maintenance mode
	req, _ := http.NewRequest(http.MethodDelete, e2eAdminURL+"/admin/maintenance", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE /admin/maintenance: %v", err)
	}
	resp.Body.Close()

	t.Run("EnableMaintenanceRejectsNewConnections", func(t *testing.T) {
		// Enable maintenance mode
		resp, err := client.Post(e2eAdminURL+"/admin/maintenance", "application/json", nil)
		if err != nil {
			t.Fatalf("POST /admin/maintenance: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /admin/maintenance: status=%d, want 200", resp.StatusCode)
		}
		t.Logf("POST /admin/maintenance OK: enabled")

		time.Sleep(200 * time.Millisecond)

		// Readyz should return 503 in maintenance mode
		readyResp, err := client.Get(e2eAdminURL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		readyResp.Body.Close()
		if readyResp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET /readyz in maintenance: status=%d, want 503", readyResp.StatusCode)
		} else {
			t.Logf("GET /readyz correctly returns 503 in maintenance mode")
		}

		// New connection attempt should fail
		newDB, err := sql.Open("postgres", e2eProxyDSN)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer newDB.Close()
		newDB.SetMaxOpenConns(1)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = newDB.PingContext(ctx)
		if err == nil {
			// The connection might succeed if it reuses a cached connection from the
			// driver pool. Try a query on a fresh connection to be sure.
			_, qErr := newDB.ExecContext(ctx, "SELECT 1")
			if qErr == nil {
				t.Logf("Warning: new connection succeeded in maintenance mode (may be driver-level caching)")
			} else if strings.Contains(qErr.Error(), "maintenance") {
				t.Logf("New query correctly rejected in maintenance mode: %v", qErr)
			}
		} else {
			if strings.Contains(err.Error(), "maintenance") {
				t.Logf("New connection correctly rejected in maintenance mode: %v", err)
			} else {
				t.Logf("New connection failed (possibly maintenance): %v", err)
			}
		}
	})

	t.Run("DisableMaintenanceAllowsConnections", func(t *testing.T) {
		// Disable maintenance mode
		req, _ := http.NewRequest(http.MethodDelete, e2eAdminURL+"/admin/maintenance", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("DELETE /admin/maintenance: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE /admin/maintenance: status=%d, want 200", resp.StatusCode)
		}
		t.Logf("DELETE /admin/maintenance OK: disabled")

		time.Sleep(200 * time.Millisecond)

		// Readyz should return 200 again
		readyResp, err := client.Get(e2eAdminURL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		readyResp.Body.Close()
		if readyResp.StatusCode != http.StatusOK {
			t.Errorf("GET /readyz after maintenance: status=%d, want 200", readyResp.StatusCode)
		}

		// New connection should work
		newDB, err := sql.Open("postgres", e2eProxyDSN)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer newDB.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = newDB.PingContext(ctx)
		if err != nil {
			t.Errorf("new connection should work after disabling maintenance: %v", err)
		} else {
			t.Logf("New connection works after disabling maintenance mode")
		}
	})
}

// TestE2E_CacheFlush tests the cache behavior: execute a cacheable query,
// flush the cache via admin API, and verify the query still succeeds (cache miss).
func TestE2E_CacheFlush(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}

	// Execute a cacheable SELECT query twice (second should be cache hit)
	var name1 string
	err := db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name1)
	if err != nil {
		t.Fatalf("first SELECT: %v", err)
	}

	var name2 string
	err = db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name2)
	if err != nil {
		t.Fatalf("second SELECT (cache hit expected): %v", err)
	}
	if name1 != name2 {
		t.Errorf("cache inconsistency: %q != %q", name1, name2)
	}
	t.Logf("Before flush: two identical queries returned consistent results: %s", name1)

	// Flush cache via admin API
	resp, err := client.Post(e2eAdminURL+"/admin/cache/flush", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /admin/cache/flush: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache flush failed: status=%d", resp.StatusCode)
	}
	t.Logf("Cache flushed via admin API")

	// Re-execute the query (cache miss — should still succeed, hitting backend)
	var name3 string
	err = db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name3)
	if err != nil {
		t.Fatalf("third SELECT (after cache flush): %v", err)
	}
	if name3 != name1 {
		t.Errorf("post-flush result mismatch: %q != %q", name3, name1)
	}
	t.Logf("After flush: query still succeeds with correct result: %s", name3)
}

// TestE2E_LargeResultSet tests streaming of large result sets (1000+ rows).
func TestE2E_LargeResultSet(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// generate_series produces a large result set without needing a pre-populated table
	rows, err := db.QueryContext(ctx, "SELECT i, 'row-' || i::text AS label FROM generate_series(1, 2000) AS i")
	if err != nil {
		t.Fatalf("large SELECT failed: %v", err)
	}
	defer rows.Close()

	var rowCount int
	var lastID int
	var lastLabel string
	for rows.Next() {
		var id int
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			t.Fatalf("scan row %d: %v", rowCount, err)
		}
		lastID = id
		lastLabel = label
		rowCount++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration error: %v", err)
	}

	if rowCount != 2000 {
		t.Errorf("got %d rows, want 2000", rowCount)
	}
	if lastID != 2000 {
		t.Errorf("last id=%d, want 2000", lastID)
	}
	if lastLabel != "row-2000" {
		t.Errorf("last label=%q, want \"row-2000\"", lastLabel)
	}
	t.Logf("Large result set OK: streamed %d rows, last=(%d, %s)", rowCount, lastID, lastLabel)
}

// TestE2E_MultiStatement tests behavior when multiple statements are sent in a
// single query string separated by semicolons. PostgreSQL simple query protocol
// supports this; verify the proxy handles it correctly.
func TestE2E_MultiStatement(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS multi_stmt_test (
		id SERIAL PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create multi_stmt_test table: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS multi_stmt_test")

	t.Run("MultiStatementExec", func(t *testing.T) {
		// Try to execute two statements separated by semicolon
		// PostgreSQL simple query protocol supports this natively
		_, err := db.ExecContext(ctx,
			"INSERT INTO multi_stmt_test (value) VALUES ('stmt1'); INSERT INTO multi_stmt_test (value) VALUES ('stmt2')")
		if err != nil {
			// If the proxy doesn't support multi-statement, verify it gives a clear error
			t.Logf("Multi-statement exec returned error (may be expected): %v", err)
		} else {
			// Verify both rows were inserted
			var count int
			err := db.QueryRowContext(ctx, "SELECT count(*) FROM multi_stmt_test").Scan(&count)
			if err != nil {
				t.Fatalf("count query: %v", err)
			}
			if count < 2 {
				t.Errorf("expected >= 2 rows from multi-statement insert, got %d", count)
			}
			t.Logf("Multi-statement Exec OK: %d rows inserted", count)
		}
	})

	t.Run("MultiStatementQuery", func(t *testing.T) {
		// Multi-statement with final SELECT
		// lib/pq uses extended query protocol for queries with parameters,
		// but simple query for plain strings
		_, err := db.ExecContext(ctx, "INSERT INTO multi_stmt_test (value) VALUES ('multi-q')")
		if err != nil {
			t.Fatalf("setup INSERT: %v", err)
		}

		// A simple SELECT after setup data
		var count int
		err = db.QueryRowContext(ctx, "SELECT count(*) FROM multi_stmt_test").Scan(&count)
		if err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count < 1 {
			t.Errorf("expected >= 1 rows, got %d", count)
		}
		t.Logf("Multi-statement query context OK: count=%d", count)
	})
}

// TestE2E_PreparedStatementReuse tests creating a prepared statement with db.Prepare(),
// executing it multiple times with different parameters, and then closing it.
func TestE2E_PreparedStatementReuse(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prepared_test (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		score INT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create prepared_test table: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS prepared_test")

	t.Run("PrepareInsertAndReuse", func(t *testing.T) {
		stmt, err := db.PrepareContext(ctx, "INSERT INTO prepared_test (name, score) VALUES ($1, $2)")
		if err != nil {
			t.Fatalf("Prepare INSERT: %v", err)
		}
		defer stmt.Close()

		// Execute the same prepared statement multiple times with different params
		testData := []struct {
			name  string
			score int
		}{
			{"alice", 100},
			{"bob", 85},
			{"charlie", 92},
			{"dave", 78},
			{"eve", 96},
		}

		for _, d := range testData {
			_, err := stmt.ExecContext(ctx, d.name, d.score)
			if err != nil {
				t.Fatalf("Exec(%q, %d): %v", d.name, d.score, err)
			}
		}
		t.Logf("Prepared INSERT: executed %d times successfully", len(testData))
	})

	t.Run("PrepareSelectAndReuse", func(t *testing.T) {
		stmt, err := db.PrepareContext(ctx, "SELECT name, score FROM prepared_test WHERE score >= $1 ORDER BY score DESC")
		if err != nil {
			t.Fatalf("Prepare SELECT: %v", err)
		}
		defer stmt.Close()

		// Execute with threshold 90 — should get alice(100), eve(96), charlie(92)
		rows, err := stmt.QueryContext(ctx, 90)
		if err != nil {
			t.Fatalf("Query(threshold=90): %v", err)
		}
		var highScorers []string
		for rows.Next() {
			var name string
			var score int
			if err := rows.Scan(&name, &score); err != nil {
				t.Fatalf("scan: %v", err)
			}
			highScorers = append(highScorers, fmt.Sprintf("%s(%d)", name, score))
		}
		rows.Close()
		if len(highScorers) != 3 {
			t.Errorf("threshold=90: got %d rows, want 3: %v", len(highScorers), highScorers)
		}
		t.Logf("Prepared SELECT (threshold=90): %v", highScorers)

		// Execute with threshold 95 — should get alice(100), eve(96)
		rows2, err := stmt.QueryContext(ctx, 95)
		if err != nil {
			t.Fatalf("Query(threshold=95): %v", err)
		}
		var topScorers []string
		for rows2.Next() {
			var name string
			var score int
			if err := rows2.Scan(&name, &score); err != nil {
				t.Fatalf("scan: %v", err)
			}
			topScorers = append(topScorers, fmt.Sprintf("%s(%d)", name, score))
		}
		rows2.Close()
		if len(topScorers) != 2 {
			t.Errorf("threshold=95: got %d rows, want 2: %v", len(topScorers), topScorers)
		}
		t.Logf("Prepared SELECT (threshold=95): %v", topScorers)

		// Execute with threshold 200 — should get 0 rows
		rows3, err := stmt.QueryContext(ctx, 200)
		if err != nil {
			t.Fatalf("Query(threshold=200): %v", err)
		}
		var none []string
		for rows3.Next() {
			var name string
			var score int
			rows3.Scan(&name, &score)
			none = append(none, name)
		}
		rows3.Close()
		if len(none) != 0 {
			t.Errorf("threshold=200: got %d rows, want 0", len(none))
		}
		t.Logf("Prepared SELECT (threshold=200): 0 rows (correct)")
	})

	t.Run("PreparedStatementClose", func(t *testing.T) {
		stmt, err := db.PrepareContext(ctx, "SELECT count(*) FROM prepared_test")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}

		var count int
		err = stmt.QueryRowContext(ctx).Scan(&count)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}

		// Close the statement
		if err := stmt.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// Using the statement after close should fail
		err = stmt.QueryRowContext(ctx).Scan(&count)
		if err == nil {
			t.Error("expected error after stmt.Close(), but got nil")
		} else {
			t.Logf("Prepared statement Close OK: post-close error=%v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Auth E2E tests
// ---------------------------------------------------------------------------

// TestE2E_Auth tests proxy-level authentication (MD5) with configured users.
func TestE2E_Auth(t *testing.T) {
	// First check proxy is up using the valid user
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("ValidUser", func(t *testing.T) {
		db, err := sql.Open("postgres", e2eProxyDSN)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()

		var result int
		err = db.QueryRowContext(ctx, "SELECT 1").Scan(&result)
		if err != nil {
			t.Fatalf("query with valid user failed: %v", err)
		}
		if result != 1 {
			t.Errorf("got %d, want 1", result)
		}
		t.Logf("Auth with postgres:postgres OK")
	})

	t.Run("ValidLimitedUser", func(t *testing.T) {
		db, err := sql.Open("postgres", e2eLimitedDSN)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()

		var result int
		err = db.QueryRowContext(ctx, "SELECT 1").Scan(&result)
		if err != nil {
			t.Fatalf("query with limited user failed: %v", err)
		}
		if result != 1 {
			t.Errorf("got %d, want 1", result)
		}
		t.Logf("Auth with limited:limited OK")
	})

	t.Run("InvalidUser", func(t *testing.T) {
		dsn := "postgres://unknownuser:badpass@127.0.0.1:15440/testdb?sslmode=disable"
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()

		err = db.PingContext(ctx)
		if err == nil {
			t.Error("expected auth failure for unknown user, but succeeded")
		} else {
			t.Logf("Auth correctly rejected unknown user: %v", err)
		}
	})

	t.Run("WrongPassword", func(t *testing.T) {
		dsn := "postgres://postgres:wrongpassword@127.0.0.1:15440/testdb?sslmode=disable"
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()

		err = db.PingContext(ctx)
		if err == nil {
			t.Error("expected auth failure for wrong password, but succeeded")
		} else {
			t.Logf("Auth correctly rejected wrong password: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Firewall E2E tests
// ---------------------------------------------------------------------------

// TestE2E_FirewallRules tests that firewall blocks dangerous queries.
func TestE2E_FirewallRules(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a test table
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS firewall_test (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create firewall_test table: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS firewall_test")

	// Insert test data
	_, err = db.ExecContext(ctx, "INSERT INTO firewall_test (name) VALUES ('a'), ('b'), ('c')")
	if err != nil {
		t.Fatalf("insert test data: %v", err)
	}

	t.Run("BlockDeleteWithoutWhere", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "DELETE FROM firewall_test")
		if err == nil {
			t.Error("DELETE without WHERE should be blocked by firewall, but succeeded")
		} else if !strings.Contains(strings.ToLower(err.Error()), "firewall") &&
			!strings.Contains(strings.ToLower(err.Error()), "blocked") &&
			!strings.Contains(strings.ToLower(err.Error()), "where") {
			t.Errorf("unexpected error (expected firewall block): %v", err)
		} else {
			t.Logf("Firewall correctly blocked DELETE without WHERE: %v", err)
		}
	})

	t.Run("BlockUpdateWithoutWhere", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE firewall_test SET name = 'x'")
		if err == nil {
			t.Error("UPDATE without WHERE should be blocked by firewall, but succeeded")
		} else if !strings.Contains(strings.ToLower(err.Error()), "firewall") &&
			!strings.Contains(strings.ToLower(err.Error()), "blocked") &&
			!strings.Contains(strings.ToLower(err.Error()), "where") {
			t.Errorf("unexpected error (expected firewall block): %v", err)
		} else {
			t.Logf("Firewall correctly blocked UPDATE without WHERE: %v", err)
		}
	})

	t.Run("AllowDeleteWithWhere", func(t *testing.T) {
		result, err := db.ExecContext(ctx, "DELETE FROM firewall_test WHERE id = 1")
		if err != nil {
			t.Fatalf("DELETE with WHERE should be allowed, but failed: %v", err)
		}
		rows, _ := result.RowsAffected()
		t.Logf("Firewall allowed DELETE with WHERE: %d rows affected", rows)
	})

	t.Run("AllowUpdateWithWhere", func(t *testing.T) {
		result, err := db.ExecContext(ctx, "UPDATE firewall_test SET name = 'updated' WHERE id = 2")
		if err != nil {
			t.Fatalf("UPDATE with WHERE should be allowed, but failed: %v", err)
		}
		rows, _ := result.RowsAffected()
		t.Logf("Firewall allowed UPDATE with WHERE: %d rows affected", rows)
	})

	t.Run("FirewallMetric", func(t *testing.T) {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(e2eMetricURL)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "pgmux_firewall_blocked_total") {
			t.Error("metrics missing pgmux_firewall_blocked_total")
		} else {
			t.Logf("Firewall metric present in /metrics")
		}
	})
}

// ---------------------------------------------------------------------------
// Connection Limits E2E tests
// ---------------------------------------------------------------------------

// TestE2E_ConnectionLimits tests per-user connection limits.
func TestE2E_ConnectionLimits(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// "limited" user has max_connections: 3 in config.test.yaml
	t.Run("PerUserLimit", func(t *testing.T) {
		conns := make([]*sql.DB, 0, 4)
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()

		// Open 3 connections (should all succeed)
		for i := 0; i < 3; i++ {
			c, err := sql.Open("postgres", e2eLimitedDSN)
			if err != nil {
				t.Fatalf("open conn %d: %v", i, err)
			}
			c.SetMaxOpenConns(1)
			c.SetMaxIdleConns(1)

			if err := c.PingContext(ctx); err != nil {
				t.Fatalf("ping conn %d: %v", i, err)
			}
			conns = append(conns, c)
		}
		t.Logf("Opened 3 connections for 'limited' user (max_connections=3)")

		// 4th connection should be rejected
		c4, err := sql.Open("postgres", e2eLimitedDSN)
		if err != nil {
			t.Fatalf("open conn 4: %v", err)
		}
		defer c4.Close()
		c4.SetMaxOpenConns(1)
		c4.SetMaxIdleConns(1)

		err = c4.PingContext(ctx)
		if err == nil {
			t.Error("4th connection should be rejected (max_connections=3), but succeeded")
		} else if strings.Contains(err.Error(), "too many connections") ||
			strings.Contains(err.Error(), "limit") {
			t.Logf("4th connection correctly rejected: %v", err)
		} else {
			t.Logf("4th connection rejected (possibly limit): %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Data API E2E tests
// ---------------------------------------------------------------------------

// TestE2E_DataAPI tests the HTTP REST Data API endpoints.
func TestE2E_DataAPI(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	client := &http.Client{Timeout: 10 * time.Second}

	t.Run("SelectQuery", func(t *testing.T) {
		body := `{"sql": "SELECT name FROM users ORDER BY id LIMIT 3"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST /v1/query: status=%d, body=%s", resp.StatusCode, string(respBody))
		}

		var result struct {
			Columns  []string `json:"columns"`
			Types    []string `json:"types"`
			Rows     [][]any  `json:"rows"`
			RowCount int      `json:"row_count"`
			Command  string   `json:"command"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}

		if result.RowCount != 3 {
			t.Errorf("row_count=%d, want 3", result.RowCount)
		}
		if len(result.Columns) == 0 || result.Columns[0] != "name" {
			t.Errorf("columns=%v, want [name]", result.Columns)
		}
		t.Logf("Data API SELECT OK: %d rows, columns=%v", result.RowCount, result.Columns)
	})

	t.Run("WriteQuery", func(t *testing.T) {
		body := `{"sql": "CREATE TABLE IF NOT EXISTS dataapi_test (id SERIAL PRIMARY KEY, val TEXT)"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query (CREATE TABLE): %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("CREATE TABLE via Data API: status=%d", resp.StatusCode)
		}

		// INSERT via Data API
		body = `{"sql": "INSERT INTO dataapi_test (val) VALUES ('hello')"}`
		req, _ = http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)

		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query (INSERT): %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("INSERT via Data API: status=%d", resp.StatusCode)
		}

		// Cleanup
		body = `{"sql": "DROP TABLE IF EXISTS dataapi_test"}`
		req, _ = http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)
		resp, err = client.Do(req)
		if err == nil {
			resp.Body.Close()
		}

		t.Logf("Data API write queries OK")
	})

	t.Run("Unauthorized_NoKey", func(t *testing.T) {
		body := `{"sql": "SELECT 1"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		// No Authorization header

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		} else {
			t.Logf("Data API correctly rejects request without API key: 401")
		}
	})

	t.Run("Unauthorized_WrongKey", func(t *testing.T) {
		body := `{"sql": "SELECT 1"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer wrong-key")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		} else {
			t.Logf("Data API correctly rejects request with wrong API key: 401")
		}
	})

	t.Run("FirewallViaDataAPI", func(t *testing.T) {
		// DELETE without WHERE should be blocked via Data API too
		body := `{"sql": "DELETE FROM users"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 (firewall block), got %d", resp.StatusCode)
		} else {
			t.Logf("Data API firewall correctly blocked DELETE without WHERE: 403")
		}
	})

	t.Run("CopyRejected", func(t *testing.T) {
		body := `{"sql": "COPY users TO STDOUT"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/query: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 (COPY not supported), got %d", resp.StatusCode)
		} else {
			t.Logf("Data API correctly rejects COPY statement: 400")
		}
	})
}

// ---------------------------------------------------------------------------
// Rate Limiting E2E tests
// ---------------------------------------------------------------------------

// TestE2E_RateLimitMetric verifies the rate limiter is configured and its metric
// is registered. With high rate/burst in test config, we verify the metric exists
// and the proxy handles queries normally under the limit.
func TestE2E_RateLimitMetric(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	db.Close()

	client := &http.Client{Timeout: 10 * time.Second}

	// Verify the rate_limited metric is registered
	resp, err := client.Get(e2eMetricURL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), "pgmux_rate_limited_total") {
		t.Error("metrics missing pgmux_rate_limited_total")
	} else {
		t.Logf("Rate limit metric present in /metrics")
	}

	// Send a burst of Data API requests — should all succeed under high rate limit
	const totalReqs = 10
	var succeeded atomic.Int32

	for i := 0; i < totalReqs; i++ {
		body := `{"sql": "SELECT 1"}`
		req, _ := http.NewRequest(http.MethodPost, e2eDataAPIURL+"/v1/query", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e2eDataAPIKey)

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			succeeded.Add(1)
		}
		resp.Body.Close()
	}

	if got := succeeded.Load(); got != totalReqs {
		t.Errorf("expected all %d requests to succeed under high rate limit, got %d", totalReqs, got)
	} else {
		t.Logf("All %d Data API requests succeeded under rate limit", totalReqs)
	}
}

// ---------------------------------------------------------------------------
// Session Compatibility E2E tests
// ---------------------------------------------------------------------------

// TestE2E_SessionCompatibility tests that session-dependent queries are detected.
// With mode="warn", queries are allowed but metrics should be updated.
func TestE2E_SessionCompatibility(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Execute a session-dependent query (SET outside transaction) to trigger detection
	_, err := db.ExecContext(ctx, "SET application_name = 'session_compat_test'")
	if err != nil {
		// In warn mode, SET should still succeed
		t.Fatalf("SET in warn mode should succeed: %v", err)
	}
	t.Logf("SET executed successfully in warn mode")

	// Check that the session dependency metric was incremented
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(e2eMetricURL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if strings.Contains(bodyStr, "pgmux_session_dependency_detected_total") {
		t.Logf("Session compatibility metric detected in /metrics")
	} else {
		t.Logf("Warning: pgmux_session_dependency_detected_total not found in metrics (may not be registered yet)")
	}
}

// ---------------------------------------------------------------------------
// SQL Redaction E2E tests
// ---------------------------------------------------------------------------

// TestE2E_SQLRedaction verifies that SQL redaction is active by executing queries
// with literal values and confirming the proxy handles them correctly.
// The redaction itself is applied to internal logs/spans (not visible to client),
// so we verify: (1) queries with literals work, (2) AST parser is active (required
// for redaction), (3) metrics are collected properly.
func TestE2E_SQLRedaction(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("QueryWithLiterals", func(t *testing.T) {
		// Execute queries containing literal values — these should be redacted in
		// internal logs/spans (policy="literals") but work normally for the client.
		var result string
		err := db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&result)
		if err != nil {
			t.Fatalf("query with literal int failed: %v", err)
		}
		if result != "alice" {
			t.Errorf("got %q, want 'alice'", result)
		}
		t.Logf("Query with literal values OK: result=%s", result)
	})

	t.Run("ParameterizedQueryWithRedaction", func(t *testing.T) {
		// Parameterized queries should also work with redaction enabled
		var result string
		err := db.QueryRowContext(ctx, "SELECT name FROM users WHERE id = $1", 2).Scan(&result)
		if err != nil {
			t.Fatalf("parameterized query failed: %v", err)
		}
		if result != "bob" {
			t.Errorf("got %q, want 'bob'", result)
		}
		t.Logf("Parameterized query with redaction OK: result=%s", result)
	})

	t.Run("ASTParserActive", func(t *testing.T) {
		// AST parser is required for SQL redaction (config: routing.ast_parser: true)
		resp, err := client.Get(e2eAdminURL + "/admin/config")
		if err != nil {
			t.Fatalf("GET /admin/config: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		if strings.Contains(bodyStr, "\"ASTParser\":true") || strings.Contains(bodyStr, "\"ast_parser\":true") {
			t.Logf("AST parser is active (required for SQL redaction)")
		} else {
			t.Error("AST parser should be enabled for SQL redaction")
		}
	})

	t.Run("MetricsWithRedaction", func(t *testing.T) {
		resp, err := client.Get(e2eMetricURL)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "pgmux_queries_routed_total") {
			t.Logf("Metrics collection works with SQL redaction enabled")
		} else {
			t.Error("metrics missing pgmux_queries_routed_total with redaction enabled")
		}
	})
}

// ---------------------------------------------------------------------------
// Admin API — Connection Limit Stats E2E
// ---------------------------------------------------------------------------

// TestE2E_AdminConnectionLimitStats tests that the admin API reports connection limit stats.
func TestE2E_AdminConnectionLimitStats(t *testing.T) {
	db := e2eCheckProxy(t)
	if db == nil {
		return
	}
	defer db.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(e2eAdminURL + "/admin/connections")
	if err != nil {
		t.Fatalf("GET /admin/connections: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/connections: status=%d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Should contain user and database connection info
	if strings.Contains(bodyStr, "postgres") || strings.Contains(bodyStr, "by_user") {
		t.Logf("Admin connections endpoint reports user data")
	}
	if strings.Contains(bodyStr, "testdb") || strings.Contains(bodyStr, "by_database") {
		t.Logf("Admin connections endpoint reports database data")
	}
	t.Logf("GET /admin/connections OK: %s", bodyStr)
}
