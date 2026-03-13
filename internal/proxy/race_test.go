package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
)

// TestServerReload_DataRace verifies that Server.Reload() is safe under concurrent access.
// With the RWMutex fix, this test must pass with `go test -race`.
func TestServerReload_DataRace(t *testing.T) {
	cfg := &config.Config{
		Proxy:  config.ProxyConfig{Listen: "127.0.0.1:0"},
		Writer: config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Readers: []config.DBConfig{
			{Host: "127.0.0.1", Port: 5433},
		},
		Pool: config.PoolConfig{MaxConnections: 10, IdleTimeout: time.Minute},
	}

	srv := NewServer(cfg)

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Goroutine 1: Continuous reads via thread-safe accessors
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Thread-safe map read via DatabaseGroup
				if dg := srv.DBGroup(srv.DefaultDBName()); dg != nil {
					_, _ = dg.ReaderPool("127.0.0.1:5433")
				}
				// Thread-safe config read
				_ = srv.getConfig().Pool.MaxConnections
			}
		}
	}()

	// Goroutine 2: Rapid config reloads
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			newCfg := &config.Config{
				Proxy:  config.ProxyConfig{Listen: "127.0.0.1:0"},
				Writer: config.DBConfig{Host: "127.0.0.1", Port: 5432},
				Readers: []config.DBConfig{
					{Host: "127.0.0.1", Port: 5433},
					{Host: "127.0.0.1", Port: 5434},
				},
				Pool: config.PoolConfig{MaxConnections: i, IdleTimeout: time.Minute},
			}
			_ = srv.Reload(newCfg)
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()
	wg.Wait()
}
