package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Proxy   ProxyConfig   `yaml:"proxy"`
	Writer  DBConfig      `yaml:"writer"`
	Readers []DBConfig    `yaml:"readers"`
	Pool    PoolConfig    `yaml:"pool"`
	Routing RoutingConfig `yaml:"routing"`
	Cache   CacheConfig   `yaml:"cache"`
	Backend BackendConfig `yaml:"backend"`
}

type BackendConfig struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
}

type ProxyConfig struct {
	Listen string `yaml:"listen"`
}

type DBConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type PoolConfig struct {
	MinConnections    int           `yaml:"min_connections"`
	MaxConnections    int           `yaml:"max_connections"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxLifetime       time.Duration `yaml:"max_lifetime"`
	ConnectionTimeout time.Duration `yaml:"connection_timeout"`
}

type RoutingConfig struct {
	ReadAfterWriteDelay time.Duration `yaml:"read_after_write_delay"`
}

type CacheConfig struct {
	Enabled        bool          `yaml:"enabled"`
	CacheTTL       time.Duration `yaml:"cache_ttl"`
	MaxCacheEntries int          `yaml:"max_cache_entries"`
	MaxResultSize   string       `yaml:"max_result_size"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Proxy.Listen == "" {
		c.Proxy.Listen = "0.0.0.0:5432"
	}
	if c.Pool.MinConnections == 0 {
		c.Pool.MinConnections = 2
	}
	if c.Pool.MaxConnections == 0 {
		c.Pool.MaxConnections = 10
	}
	if c.Pool.IdleTimeout == 0 {
		c.Pool.IdleTimeout = 10 * time.Minute
	}
	if c.Pool.MaxLifetime == 0 {
		c.Pool.MaxLifetime = time.Hour
	}
	if c.Pool.ConnectionTimeout == 0 {
		c.Pool.ConnectionTimeout = 5 * time.Second
	}
	if c.Routing.ReadAfterWriteDelay == 0 {
		c.Routing.ReadAfterWriteDelay = 500 * time.Millisecond
	}
	if c.Cache.CacheTTL == 0 {
		c.Cache.CacheTTL = 10 * time.Second
	}
	if c.Cache.MaxCacheEntries == 0 {
		c.Cache.MaxCacheEntries = 10000
	}
	if c.Cache.MaxResultSize == "" {
		c.Cache.MaxResultSize = "1MB"
	}
	if c.Backend.User == "" {
		c.Backend.User = "postgres"
	}
	if c.Backend.Database == "" {
		c.Backend.Database = "postgres"
	}
}

func (c *Config) validate() error {
	if c.Writer.Host == "" {
		return fmt.Errorf("writer.host is required")
	}
	if c.Writer.Port <= 0 || c.Writer.Port > 65535 {
		return fmt.Errorf("writer.port must be between 1 and 65535, got %d", c.Writer.Port)
	}
	if len(c.Readers) == 0 {
		return fmt.Errorf("at least one reader is required")
	}
	for i, r := range c.Readers {
		if r.Host == "" {
			return fmt.Errorf("readers[%d].host is required", i)
		}
		if r.Port <= 0 || r.Port > 65535 {
			return fmt.Errorf("readers[%d].port must be between 1 and 65535, got %d", i, r.Port)
		}
	}
	if c.Pool.MinConnections < 0 {
		return fmt.Errorf("pool.min_connections must be >= 0, got %d", c.Pool.MinConnections)
	}
	if c.Pool.MaxConnections < 1 {
		return fmt.Errorf("pool.max_connections must be >= 1, got %d", c.Pool.MaxConnections)
	}
	if c.Pool.MinConnections > c.Pool.MaxConnections {
		return fmt.Errorf("pool.min_connections (%d) must be <= pool.max_connections (%d)", c.Pool.MinConnections, c.Pool.MaxConnections)
	}
	return nil
}
