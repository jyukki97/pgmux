package cache

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Invalidator broadcasts and receives cache invalidation events across proxy instances.
type Invalidator struct {
	rdb     *redis.Client
	channel string
	cache   *Cache
}

// NewInvalidator creates a Redis Pub/Sub based cache invalidator.
func NewInvalidator(redisAddr, channel string, cache *Cache) (*Invalidator, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	// Verify connection (with timeout to prevent startup hang)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, err
	}

	return &Invalidator{
		rdb:     rdb,
		channel: channel,
		cache:   cache,
	}, nil
}

// Publish sends a cache invalidation event for the given tables.
// Message format: "table1,table2,..." or "*" for full flush.
func (inv *Invalidator) Publish(ctx context.Context, tables []string) {
	msg := strings.Join(tables, ",")
	if err := inv.rdb.Publish(ctx, inv.channel, msg).Err(); err != nil {
		slog.Error("cache invalidation publish failed", "error", err, "tables", tables)
	}
}

// PublishFlushAll sends a full cache flush event.
func (inv *Invalidator) PublishFlushAll(ctx context.Context) {
	if err := inv.rdb.Publish(ctx, inv.channel, "*").Err(); err != nil {
		slog.Error("cache invalidation publish flush-all failed", "error", err)
	}
}

// Subscribe listens for invalidation events and applies them to the local cache.
// Blocks until ctx is cancelled. Run in a goroutine.
func (inv *Invalidator) Subscribe(ctx context.Context) {
	sub := inv.rdb.Subscribe(ctx, inv.channel)
	defer sub.Close()

	ch := sub.Channel()
	slog.Info("cache invalidation subscriber started", "channel", inv.channel)

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			inv.handleMessage(msg.Payload)
		}
	}
}

func (inv *Invalidator) handleMessage(payload string) {
	if payload == "*" {
		inv.cache.FlushAll()
		slog.Debug("cache invalidation: full flush from remote")
		return
	}

	tables := strings.Split(payload, ",")
	for _, table := range tables {
		table = strings.TrimSpace(table)
		if table != "" {
			inv.cache.InvalidateTable(table)
		}
	}
	slog.Debug("cache invalidation: tables from remote", "tables", tables)
}

// Close closes the Redis connection.
func (inv *Invalidator) Close() error {
	return inv.rdb.Close()
}
