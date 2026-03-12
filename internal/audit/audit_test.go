package audit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowQueryDetection(t *testing.T) {
	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 100 * time.Millisecond,
		LogAllQueries:      false,
	})
	defer l.Close()

	// Fast query — should NOT count as slow
	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT 1",
		DurationMS: 10,
		Target:     "reader",
	})

	// Slow query — should count as slow
	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT * FROM large_table",
		DurationMS: 200,
		Target:     "reader",
	})

	// Give async processing time
	time.Sleep(50 * time.Millisecond)

	slow, _, _ := l.Stats()
	if slow != 1 {
		t.Errorf("expected 1 slow query, got %d", slow)
	}
}

func TestLogAllQueries(t *testing.T) {
	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 1 * time.Second,
		LogAllQueries:      true,
	})
	defer l.Close()

	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT 1",
		DurationMS: 1,
		Target:     "reader",
	})

	// Give async processing time
	time.Sleep(50 * time.Millisecond)

	slow, _, _ := l.Stats()
	if slow != 0 {
		t.Errorf("expected 0 slow queries, got %d", slow)
	}
}

func TestWebhookSend(t *testing.T) {
	var received atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}

		if text, ok := payload["text"].(string); !ok || text != "[pgmux] Slow Query Detected" {
			t.Errorf("unexpected text: %v", payload["text"])
		}

		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 100 * time.Millisecond,
		LogAllQueries:      false,
		Webhook: WebhookConfig{
			Enabled: true,
			URL:     ts.URL,
			Timeout: 5 * time.Second,
		},
	})
	defer l.Close()

	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT * FROM slow_table",
		DurationMS: 500,
		Target:     "writer",
		User:       "postgres",
		SourceIP:   "192.168.1.1:5432",
	})

	time.Sleep(100 * time.Millisecond)

	if received.Load() != 1 {
		t.Errorf("expected 1 webhook call, got %d", received.Load())
	}

	sent, webhookSent, webhookErr := l.Stats()
	_ = sent
	if webhookSent != 1 {
		t.Errorf("expected webhookSent=1, got %d", webhookSent)
	}
	if webhookErr != 0 {
		t.Errorf("expected webhookErr=0, got %d", webhookErr)
	}
}

func TestWebhookRateLimiting(t *testing.T) {
	var received atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 100 * time.Millisecond,
		Webhook: WebhookConfig{
			Enabled:       true,
			URL:           ts.URL,
			Timeout:       5 * time.Second,
			DedupInterval: 10 * time.Second, // Long interval for testing
		},
	})
	defer l.Close()

	// Same query twice — second should be deduplicated
	for i := 0; i < 3; i++ {
		l.Log(Event{
			Timestamp:  time.Now(),
			Query:      "SELECT * FROM same_slow_query",
			DurationMS: 500,
			Target:     "reader",
		})
	}

	time.Sleep(100 * time.Millisecond)

	if received.Load() != 1 {
		t.Errorf("expected 1 webhook (deduped), got %d", received.Load())
	}
}

func TestWebhookDifferentQueries(t *testing.T) {
	var received atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 100 * time.Millisecond,
		Webhook: WebhookConfig{
			Enabled:       true,
			URL:           ts.URL,
			Timeout:       5 * time.Second,
			DedupInterval: 10 * time.Second,
		},
	})
	defer l.Close()

	// Different queries should both trigger webhooks
	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT * FROM table_a",
		DurationMS: 500,
		Target:     "reader",
	})
	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT * FROM table_b",
		DurationMS: 600,
		Target:     "writer",
	})

	time.Sleep(100 * time.Millisecond)

	if received.Load() != 2 {
		t.Errorf("expected 2 webhook calls (different queries), got %d", received.Load())
	}
}

func TestWebhookError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 100 * time.Millisecond,
		Webhook: WebhookConfig{
			Enabled: true,
			URL:     ts.URL,
			Timeout: 5 * time.Second,
		},
	})
	defer l.Close()

	l.Log(Event{
		Timestamp:  time.Now(),
		Query:      "SELECT * FROM error_table",
		DurationMS: 500,
		Target:     "reader",
	})

	time.Sleep(100 * time.Millisecond)

	_, _, webhookErr := l.Stats()
	if webhookErr != 1 {
		t.Errorf("expected webhookErr=1, got %d", webhookErr)
	}
}

func TestTruncateQuery(t *testing.T) {
	short := "SELECT 1"
	if truncateQuery(short, 100) != short {
		t.Errorf("short query should not be truncated")
	}

	long := "SELECT " + string(make([]byte, 200))
	result := truncateQuery(long, 50)
	if len(result) != 53 { // 50 + "..."
		t.Errorf("expected truncated length 53, got %d", len(result))
	}
	if result[len(result)-3:] != "..." {
		t.Errorf("expected '...' suffix")
	}
}

func TestCloseDrainsPendingEvents(t *testing.T) {
	l := New(Config{
		Enabled:            true,
		SlowQueryThreshold: 100 * time.Millisecond,
	})

	for i := 0; i < 10; i++ {
		l.Log(Event{
			Timestamp:  time.Now(),
			Query:      "SELECT * FROM big",
			DurationMS: 500,
			Target:     "reader",
		})
	}

	l.Close()

	slow, _, _ := l.Stats()
	if slow != 10 {
		t.Errorf("expected 10 slow queries after drain, got %d", slow)
	}
}
