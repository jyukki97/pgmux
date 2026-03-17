// Package cache implements an in-memory LRU cache with TTL and table-level invalidation for query results.
package cache

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

type entry struct {
	key       uint64
	result    []byte
	tables    []string
	expiresAt time.Time
}

type Cache struct {
	mu         sync.RWMutex
	items      map[uint64]*list.Element
	evictList  *list.List
	maxEntries int
	ttl        time.Duration
	maxSize    int // max result size in bytes

	// Table → cache keys reverse index for invalidation
	tableIndex map[string]map[uint64]struct{}
}

type Config struct {
	MaxEntries int
	TTL        time.Duration
	MaxSize    int // max single result size in bytes
}

func New(cfg Config) *Cache {
	return &Cache{
		items:      make(map[uint64]*list.Element),
		evictList:  list.New(),
		maxEntries: cfg.MaxEntries,
		ttl:        cfg.TTL,
		maxSize:    cfg.MaxSize,
		tableIndex: make(map[string]map[uint64]struct{}),
	}
}

// Cache key namespace constants to prevent cross-path collisions.
// Different response formats (PG wire, JSON, extended wire) must use
// different namespaces so the same SQL doesn't return the wrong format.
const (
	NSProxyWire uint64 = 0                    // proxy simple query (default)
	NSDataAPI   uint64 = 0xa5a5a5a5a5a5a5a5  // Data API JSON responses
	NSExtended  uint64 = 0x5a5a5a5a5a5a5a5a  // extended query wire responses
)

// WithNamespace mixes a namespace into a cache key to prevent collisions
// between different response formats sharing the same cache.
func WithNamespace(key uint64, ns uint64) uint64 {
	return key ^ ns
}

// CacheKey generates a hash key from query text and parameters.
func CacheKey(query string, params ...any) uint64 {
	h := fnv.New64a()
	h.Write([]byte(query))
	for _, p := range params {
		if s, ok := p.(string); ok {
			h.Write([]byte(s))
		}
	}
	return h.Sum64()
}

// Get retrieves a cached result. Returns nil if not found or expired.
func (c *Cache) Get(key uint64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil
	}

	e := elem.Value.(*entry)

	// Check TTL
	if time.Now().After(e.expiresAt) {
		c.removeElement(elem)
		return nil
	}

	// Move to front (most recently used)
	c.evictList.MoveToFront(elem)
	return e.result
}

// Set stores a query result in the cache.
func (c *Cache) Set(key uint64, result []byte, tables []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Skip if result too large
	if c.maxSize > 0 && len(result) > c.maxSize {
		return
	}

	// Update existing entry
	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		e := elem.Value.(*entry)
		// Remove stale table references before updating
		c.removeTableIndex(key, e.tables)
		e.result = result
		e.tables = tables
		e.expiresAt = time.Now().Add(c.ttl)
		c.updateTableIndex(key, tables)
		return
	}

	// Evict if at capacity
	if c.maxEntries > 0 && c.evictList.Len() >= c.maxEntries {
		c.evictOldest()
	}

	// Add new entry
	e := &entry{
		key:       key,
		result:    result,
		tables:    tables,
		expiresAt: time.Now().Add(c.ttl),
	}
	elem := c.evictList.PushFront(e)
	c.items[key] = elem
	c.updateTableIndex(key, tables)
}

// InvalidateTable removes all cache entries that reference the given table.
func (c *Cache) InvalidateTable(table string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	keys, ok := c.tableIndex[table]
	if !ok {
		return
	}

	for key := range keys {
		if elem, ok := c.items[key]; ok {
			c.removeElement(elem)
		}
	}

	delete(c.tableIndex, table)
}

// FlushAll removes all entries from the cache.
func (c *Cache) FlushAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[uint64]*list.Element)
	c.evictList.Init()
	c.tableIndex = make(map[string]map[uint64]struct{})
}

// Len returns the number of items in the cache.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.evictList.Len()
}

func (c *Cache) removeElement(elem *list.Element) {
	e := elem.Value.(*entry)
	c.evictList.Remove(elem)
	delete(c.items, e.key)

	// Clean up table index
	for _, table := range e.tables {
		if keys, ok := c.tableIndex[table]; ok {
			delete(keys, e.key)
			if len(keys) == 0 {
				delete(c.tableIndex, table)
			}
		}
	}
}

func (c *Cache) evictOldest() {
	elem := c.evictList.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}

func (c *Cache) updateTableIndex(key uint64, tables []string) {
	for _, table := range tables {
		if _, ok := c.tableIndex[table]; !ok {
			c.tableIndex[table] = make(map[uint64]struct{})
		}
		c.tableIndex[table][key] = struct{}{}
	}
}

// removeTableIndex removes a key from the table index for the given tables.
func (c *Cache) removeTableIndex(key uint64, tables []string) {
	for _, table := range tables {
		if keys, ok := c.tableIndex[table]; ok {
			delete(keys, key)
			if len(keys) == 0 {
				delete(c.tableIndex, table)
			}
		}
	}
}
