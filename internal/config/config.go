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
	Metrics MetricsConfig `yaml:"metrics"`
	Admin   AdminConfig   `yaml:"admin"`
	TLS            TLSConfig            `yaml:"tls"`
	Auth           AuthConfig           `yaml:"auth"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	RateLimit      RateLimitConfig      `yaml:"rate_limit"`
	Firewall       FirewallConfig       `yaml:"firewall"`
	Audit          AuditConfig          `yaml:"audit"`
	DataAPI        DataAPIConfig        `yaml:"data_api"`
	ConfigOptions  ConfigOptionsConfig  `yaml:"config"`
}

type ConfigOptionsConfig struct {
	Watch bool `yaml:"watch"`
}

type DataAPIConfig struct {
	Enabled bool     `yaml:"enabled"`
	Listen  string   `yaml:"listen"`
	APIKeys []string `yaml:"api_keys"`
}

type AuditConfig struct {
	Enabled            bool                `yaml:"enabled"`
	SlowQueryThreshold time.Duration       `yaml:"slow_query_threshold"`
	LogAllQueries      bool                `yaml:"log_all_queries"`
	Webhook            AuditWebhookConfig  `yaml:"webhook"`
}

type AuditWebhookConfig struct {
	Enabled bool          `yaml:"enabled"`
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

type FirewallConfig struct {
	Enabled                 bool `yaml:"enabled"`
	BlockDeleteWithoutWhere bool `yaml:"block_delete_without_where"`
	BlockUpdateWithoutWhere bool `yaml:"block_update_without_where"`
	BlockDropTable          bool `yaml:"block_drop_table"`
	BlockTruncate           bool `yaml:"block_truncate"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

type AdminConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type AuthConfig struct {
	Enabled bool       `yaml:"enabled"`
	Users   []AuthUser `yaml:"users"`
}

type AuthUser struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type CircuitBreakerConfig struct {
	Enabled        bool          `yaml:"enabled"`
	ErrorThreshold float64       `yaml:"error_threshold"` // 0.0-1.0
	OpenDuration   time.Duration `yaml:"open_duration"`
	HalfOpenMax    int           `yaml:"half_open_max"`
	WindowSize     int           `yaml:"window_size"`
}

type RateLimitConfig struct {
	Enabled bool    `yaml:"enabled"`
	Rate    float64 `yaml:"rate"`  // queries per second
	Burst   int     `yaml:"burst"` // max burst size
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
	ResetQuery        string        `yaml:"reset_query"`
}

type RoutingConfig struct {
	ReadAfterWriteDelay time.Duration `yaml:"read_after_write_delay"`
	CausalConsistency   bool          `yaml:"causal_consistency"`
	ASTParser           bool          `yaml:"ast_parser"`
}

type CacheConfig struct {
	Enabled         bool                   `yaml:"enabled"`
	CacheTTL        time.Duration          `yaml:"cache_ttl"`
	MaxCacheEntries int                    `yaml:"max_cache_entries"`
	MaxResultSize   string                 `yaml:"max_result_size"`
	Invalidation    CacheInvalidationConfig `yaml:"invalidation"`
}

type CacheInvalidationConfig struct {
	Mode      string `yaml:"mode"`       // "local" (default) or "pubsub"
	RedisAddr string `yaml:"redis_addr"` // Redis address for pubsub mode
	Channel   string `yaml:"channel"`    // Redis channel name
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
	if c.Pool.ResetQuery == "" {
		c.Pool.ResetQuery = "DISCARD ALL"
	}
	if c.Cache.Invalidation.Mode == "" {
		c.Cache.Invalidation.Mode = "local"
	}
	if c.Cache.Invalidation.Channel == "" {
		c.Cache.Invalidation.Channel = "pgmux:invalidate"
	}
	if c.Backend.User == "" {
		c.Backend.User = "postgres"
	}
	if c.Backend.Database == "" {
		c.Backend.Database = "postgres"
	}
	if c.Metrics.Listen == "" {
		c.Metrics.Listen = "0.0.0.0:9090"
	}
	if c.Admin.Listen == "" {
		c.Admin.Listen = "0.0.0.0:9091"
	}
	if c.RateLimit.Rate <= 0 {
		c.RateLimit.Rate = 1000
	}
	if c.RateLimit.Burst <= 0 {
		c.RateLimit.Burst = 100
	}
	if c.CircuitBreaker.ErrorThreshold <= 0 {
		c.CircuitBreaker.ErrorThreshold = 0.5
	}
	if c.CircuitBreaker.OpenDuration <= 0 {
		c.CircuitBreaker.OpenDuration = 10 * time.Second
	}
	if c.CircuitBreaker.HalfOpenMax <= 0 {
		c.CircuitBreaker.HalfOpenMax = 3
	}
	if c.CircuitBreaker.WindowSize <= 0 {
		c.CircuitBreaker.WindowSize = 10
	}
	if c.Audit.SlowQueryThreshold == 0 {
		c.Audit.SlowQueryThreshold = 500 * time.Millisecond
	}
	if c.Audit.Webhook.Timeout == 0 {
		c.Audit.Webhook.Timeout = 5 * time.Second
	}
	if c.DataAPI.Listen == "" {
		c.DataAPI.Listen = "0.0.0.0:8080"
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
	if c.TLS.Enabled {
		if c.TLS.CertFile == "" {
			return fmt.Errorf("tls.cert_file is required when tls is enabled")
		}
		if c.TLS.KeyFile == "" {
			return fmt.Errorf("tls.key_file is required when tls is enabled")
		}
		if _, err := os.Stat(c.TLS.CertFile); err != nil {
			return fmt.Errorf("tls.cert_file: %w", err)
		}
		if _, err := os.Stat(c.TLS.KeyFile); err != nil {
			return fmt.Errorf("tls.key_file: %w", err)
		}
	}
	if c.Auth.Enabled && len(c.Auth.Users) == 0 {
		return fmt.Errorf("auth.users is required when auth is enabled")
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
