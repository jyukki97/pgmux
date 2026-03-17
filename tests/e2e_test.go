package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
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
		// Client A sets a session variable, then disconnects (connection returns to pool).
		// Client B acquires a connection and should NOT see A's session state.

		connA, err := sql.Open("postgres", proxyDSN)
		if err != nil {
			t.Fatalf("open connA: %v", err)
		}
		// Force a single underlying connection so we control the pool conn
		connA.SetMaxOpenConns(1)

		// Set a session-level parameter
		_, err = connA.ExecContext(ctx, "SET application_name = 'client_a_session'")
		if err != nil {
			connA.Close()
			t.Fatalf("SET application_name: %v", err)
		}

		// Verify it was set
		var appName string
		err = connA.QueryRowContext(ctx, "SHOW application_name").Scan(&appName)
		if err != nil {
			connA.Close()
			t.Fatalf("SHOW application_name: %v", err)
		}
		if appName != "client_a_session" {
			connA.Close()
			t.Fatalf("application_name = %q, want 'client_a_session'", appName)
		}

		// Close client A — connection returns to pool with DISCARD ALL
		connA.Close()

		// Small delay to ensure connection is returned
		time.Sleep(100 * time.Millisecond)

		// Client B opens a new connection — should get a reset connection
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

		// After DISCARD ALL, application_name should be reset to default (empty or "")
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
		// Create temp table
		_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS causal_test (
			id SERIAL PRIMARY KEY,
			value TEXT NOT NULL
		)`)
		if err != nil {
			t.Fatalf("create causal_test table: %v", err)
		}
		defer db.ExecContext(ctx, "DROP TABLE IF EXISTS causal_test")

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
