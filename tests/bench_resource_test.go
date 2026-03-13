package tests

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/pool"
)

// === Connection Pool Concurrent Stress Test ===

func BenchmarkPool_AcquireRelease_Parallel(b *testing.B) {
	// Create a mock TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Keep connection alive
			go func() {
				buf := make([]byte, 1)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	p, err := pool.New(pool.Config{
		Addr:              ln.Addr().String(),
		MinConnections:    5,
		MaxConnections:    20,
		IdleTimeout:       time.Minute,
		MaxLifetime:       time.Hour,
		ConnectionTimeout: 5 * time.Second,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := p.Acquire(ctx)
			if err != nil {
				b.Error(err)
				return
			}
			p.Release(conn)
		}
	})
}

// === Pool Exhaustion & Recovery Test ===

func TestPool_ExhaustionRecovery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	maxConn := 5
	p, err := pool.New(pool.Config{
		Addr:              ln.Addr().String(),
		MinConnections:    2,
		MaxConnections:    maxConn,
		IdleTimeout:       time.Minute,
		MaxLifetime:       time.Hour,
		ConnectionTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()

	// Exhaust the pool
	held := make([]*pool.Conn, 0, maxConn)
	for i := 0; i < maxConn; i++ {
		conn, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		held = append(held, conn)
	}

	// Next acquire should timeout
	_, err = p.Acquire(ctx)
	if err == nil {
		t.Fatal("expected timeout when pool exhausted")
	}

	// Release one — next acquire should succeed
	p.Release(held[0])
	held = held[1:]

	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	p.Release(conn)

	// Release all
	for _, c := range held {
		p.Release(c)
	}

	numOpen, numIdle := p.Stats()
	if numOpen > maxConn {
		t.Errorf("numOpen=%d exceeds max=%d", numOpen, maxConn)
	}
	t.Logf("pool stats after recovery: open=%d idle=%d", numOpen, numIdle)
}

// === Pool Concurrent Stress with Discard ===

func TestPool_ConcurrentStress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	p, err := pool.New(pool.Config{
		Addr:              ln.Addr().String(),
		MinConnections:    2,
		MaxConnections:    10,
		IdleTimeout:       time.Minute,
		MaxLifetime:       time.Hour,
		ConnectionTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var (
		wg        sync.WaitGroup
		acquired  atomic.Int64
		released  atomic.Int64
		discarded atomic.Int64
		errors    atomic.Int64
	)

	// 50 goroutines competing for 10 connections
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for ctx.Err() == nil {
				conn, err := p.Acquire(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					errors.Add(1)
					continue
				}
				acquired.Add(1)

				// Simulate work
				time.Sleep(time.Duration(id%5) * time.Millisecond)

				// 10% discard (simulate broken connections)
				if id%10 == 0 {
					p.Discard(conn)
					discarded.Add(1)
				} else {
					p.Release(conn)
					released.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()

	numOpen, numIdle := p.Stats()
	t.Logf("results: acquired=%d released=%d discarded=%d errors=%d",
		acquired.Load(), released.Load(), discarded.Load(), errors.Load())
	t.Logf("pool stats: open=%d idle=%d", numOpen, numIdle)

	if numOpen > 10 {
		t.Errorf("pool leaked: numOpen=%d > maxConnections=10", numOpen)
	}
	if errors.Load() > acquired.Load()/2 {
		t.Errorf("too many errors: %d out of %d acquires", errors.Load(), acquired.Load())
	}
}

// === Memory Footprint Test ===

func TestMemoryFootprint(t *testing.T) {
	// Use TotalAlloc (monotonically increasing) for accurate measurement
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// Create cache with 10,000 entries (default config)
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	// Fill with realistic query results (~500 bytes avg)
	result := make([]byte, 500)
	for i := 0; i < 10000; i++ {
		key := cache.CacheKey(fmt.Sprintf("SELECT * FROM users WHERE id = %d", i))
		c.Set(key, result, []string{"users"})
	}
	runtime.KeepAlive(c)

	runtime.GC()
	var afterCache runtime.MemStats
	runtime.ReadMemStats(&afterCache)

	cacheHeapMB := float64(afterCache.HeapInuse-before.HeapInuse) / 1024 / 1024
	cacheTotalMB := float64(afterCache.TotalAlloc-before.TotalAlloc) / 1024 / 1024
	t.Logf("cache (10k entries × 500B): heap_inuse=%.1f MB, total_alloc=%.1f MB", cacheHeapMB, cacheTotalMB)

	// Create pool with real TCP connections
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	runtime.GC()
	var beforePool runtime.MemStats
	runtime.ReadMemStats(&beforePool)

	p, err := pool.New(pool.Config{
		Addr:           ln.Addr().String(),
		MinConnections: 10,
		MaxConnections: 50,
		IdleTimeout:    time.Minute,
		MaxLifetime:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	runtime.KeepAlive(p)

	runtime.GC()
	var afterPool runtime.MemStats
	runtime.ReadMemStats(&afterPool)

	poolKB := float64(afterPool.TotalAlloc-beforePool.TotalAlloc) / 1024
	t.Logf("pool (10 pre-created conns): total_alloc=%.1f KB", poolKB)

	t.Logf("current heap: %.1f MB", float64(afterPool.HeapInuse)/1024/1024)
	t.Logf("goroutines: %d", runtime.NumGoroutine())
	t.Logf("binary note: 32 MB on disk (includes cgo pg_query C parser)")
}
