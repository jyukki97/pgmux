package audit

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditLogger_WebhookDoesNotBlockLogging(t *testing.T) {
	// Create a mock server that takes 2 seconds to respond
	var webhookCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookCalls.Add(1)
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := Config{
		Enabled:            true,
		SlowQueryThreshold: 10 * time.Millisecond,
		LogAllQueries:      true,
		Webhook: WebhookConfig{
			Enabled: true,
			URL:     ts.URL,
			Timeout: 5 * time.Second,
		},
	}

	logger := New(cfg)
	defer logger.Close()

	// Send a slow query that triggers a webhook
	logger.Log(Event{DurationMS: 100, Query: "SELECT * FROM delay1"})

	// Wait for the processor to pick up the event and fire the webhook goroutine
	time.Sleep(100 * time.Millisecond)

	// Now send 1100 normal events while the webhook is still in-flight.
	// With the fix, the processor goroutine is NOT blocked, so it should drain
	// these events from the channel without dropping them.
	sent := 1100
	for i := 0; i < sent; i++ {
		logger.Log(Event{DurationMS: 5, Query: "SELECT * FROM fast"})
	}

	// Give the processor time to drain all events (should be fast since no blocking)
	time.Sleep(500 * time.Millisecond)

	// Check channel is drained — if the processor was blocked, events would pile up
	remaining := len(logger.eventCh)
	if remaining > 10 {
		t.Errorf("Expected channel to be mostly drained, but %d events remain (processor was likely blocked)", remaining)
	}
}
