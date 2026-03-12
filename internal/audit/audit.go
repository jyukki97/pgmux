package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Event represents a single audit log entry.
type Event struct {
	Timestamp  time.Time `json:"timestamp"`
	EventType  string    `json:"event"`       // "query" or "slow_query"
	User       string    `json:"user"`
	SourceIP   string    `json:"source_ip"`
	Query      string    `json:"query"`
	DurationMS float64   `json:"duration_ms"`
	Target     string    `json:"target"` // "writer" or "reader"
	Cached     bool      `json:"cached"`
}

// Config holds audit logging configuration.
type Config struct {
	Enabled            bool
	SlowQueryThreshold time.Duration
	LogAllQueries      bool
	Webhook            WebhookConfig
}

// WebhookConfig holds webhook notification configuration.
type WebhookConfig struct {
	Enabled       bool
	URL           string
	Timeout       time.Duration
	DedupInterval time.Duration // dedup window for same query; default 1m
}

// Logger is the audit logger that handles structured logging and webhook notifications.
type Logger struct {
	cfg         Config
	eventCh     chan Event
	httpClient  *http.Client
	slowCount   int64
	webhookSent int64
	webhookErr  int64
	mu          sync.RWMutex

	// Dedup: track last webhook time per normalized query prefix
	lastWebhook     map[string]time.Time
	lastWebhookMu   sync.Mutex
	webhookInterval time.Duration

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates a new AuditLogger. Call Close() when done.
func New(cfg Config) *Logger {
	dedupInterval := cfg.Webhook.DedupInterval
	if dedupInterval == 0 {
		dedupInterval = time.Minute
	}
	l := &Logger{
		cfg:             cfg,
		eventCh:         make(chan Event, 1024),
		lastWebhook:     make(map[string]time.Time),
		webhookInterval: dedupInterval,
		stopCh:          make(chan struct{}),
	}

	if cfg.Webhook.Enabled && cfg.Webhook.URL != "" {
		timeout := cfg.Webhook.Timeout
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		l.httpClient = &http.Client{Timeout: timeout}
	}

	l.wg.Add(1)
	go l.processEvents()

	l.wg.Add(1)
	go l.cleanupWebhookDedup()

	return l
}

// Log sends an audit event for processing. Non-blocking; drops events if buffer is full.
func (l *Logger) Log(e Event) {
	select {
	case l.eventCh <- e:
	default:
		slog.Warn("audit event channel full, dropping event")
	}
}

// Close stops the audit logger and waits for pending events to drain.
func (l *Logger) Close() {
	close(l.stopCh)
	l.wg.Wait()
}

// Stats returns audit statistics.
func (l *Logger) Stats() (slowQueries int64, webhookSent int64, webhookErrors int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.slowCount, l.webhookSent, l.webhookErr
}

func (l *Logger) processEvents() {
	defer l.wg.Done()
	for {
		select {
		case <-l.stopCh:
			// Drain remaining events
			for {
				select {
				case e := <-l.eventCh:
					l.handleEvent(e)
				default:
					return
				}
			}
		case e := <-l.eventCh:
			l.handleEvent(e)
		}
	}
}

func (l *Logger) handleEvent(e Event) {
	isSlowQuery := e.DurationMS >= float64(l.cfg.SlowQueryThreshold.Milliseconds())

	if isSlowQuery {
		e.EventType = "slow_query"
		l.mu.Lock()
		l.slowCount++
		l.mu.Unlock()

		slog.Warn("slow query detected",
			"event", e.EventType,
			"user", e.User,
			"source_ip", e.SourceIP,
			"query", truncateQuery(e.Query, 500),
			"duration_ms", e.DurationMS,
			"target", e.Target,
			"cached", e.Cached,
		)

		if l.httpClient != nil {
			l.wg.Add(1)
			go func() {
				defer l.wg.Done()
				l.sendWebhook(e)
			}()
		}
	} else if l.cfg.LogAllQueries {
		e.EventType = "query"
		slog.Info("audit query",
			"event", e.EventType,
			"user", e.User,
			"source_ip", e.SourceIP,
			"query", truncateQuery(e.Query, 500),
			"duration_ms", e.DurationMS,
			"target", e.Target,
			"cached", e.Cached,
		)
	}
}

func (l *Logger) sendWebhook(e Event) {
	// Rate limit: skip if same query prefix was sent within interval
	queryKey := truncateQuery(e.Query, 100)
	l.lastWebhookMu.Lock()
	if last, ok := l.lastWebhook[queryKey]; ok && time.Since(last) < l.webhookInterval {
		l.lastWebhookMu.Unlock()
		return
	}
	l.lastWebhook[queryKey] = time.Now()
	l.lastWebhookMu.Unlock()

	payload := map[string]any{
		"text": "[pgmux] Slow Query Detected",
		"attachments": []map[string]any{
			{
				"color": "danger",
				"fields": []map[string]any{
					{"title": "Query", "value": truncateQuery(e.Query, 500), "short": false},
					{"title": "Duration", "value": fmt.Sprintf("%.1fms", e.DurationMS), "short": true},
					{"title": "Target", "value": e.Target, "short": true},
					{"title": "User", "value": e.User, "short": true},
					{"title": "Source IP", "value": e.SourceIP, "short": true},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("audit webhook marshal", "error", err)
		return
	}

	resp, err := l.httpClient.Post(l.cfg.Webhook.URL, "application/json", bytes.NewReader(body))
	l.mu.Lock()
	if err != nil {
		l.webhookErr++
		l.mu.Unlock()
		slog.Error("audit webhook send", "error", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		l.webhookErr++
		l.mu.Unlock()
		slog.Error("audit webhook response", "status", resp.StatusCode)
		return
	}

	l.webhookSent++
	l.mu.Unlock()
	slog.Debug("audit webhook sent", "query", truncateQuery(e.Query, 100))
}

// cleanupWebhookDedup periodically removes expired entries from lastWebhook map
// to prevent unbounded memory growth.
func (l *Logger) cleanupWebhookDedup() {
	defer l.wg.Done()
	ticker := time.NewTicker(l.webhookInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			l.lastWebhookMu.Lock()
			now := time.Now()
			for key, last := range l.lastWebhook {
				if now.Sub(last) >= l.webhookInterval {
					delete(l.lastWebhook, key)
				}
			}
			l.lastWebhookMu.Unlock()
		}
	}
}

func truncateQuery(q string, maxLen int) string {
	if len(q) <= maxLen {
		return q
	}
	return q[:maxLen] + "..."
}
