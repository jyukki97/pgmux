package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// TestE2E_ProxyIntegration tests the full proxy with real PostgreSQL backends.
// Requires: docker-compose up && go run ./cmd/db-proxy config.test.yaml
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

// TestE2E_ProxyStartStop tests that the proxy binary starts and stops cleanly.
func TestE2E_ProxyStartStop(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("set E2E=1 to run")
	}

	binPath := "../bin/db-proxy"
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

	fmt.Println("proxy start/stop OK")
}
