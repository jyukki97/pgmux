package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestReadMessage_MaxSizeExceeded(t *testing.T) {
	// Simulate a malicious client sending a 1GB length header
	buf := new(bytes.Buffer)
	buf.WriteByte('Q')
	binary.Write(buf, binary.BigEndian, int32(1024*1024*1024)) // 1GB

	_, err := ReadMessage(buf)
	if err == nil {
		t.Fatal("expected error for extremely large message, got nil")
	}
	if !strings.Contains(err.Error(), "message too large") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadMessage_ExactlyMaxSize(t *testing.T) {
	// A message at exactly MaxMessageSize should be accepted (not rejected)
	// We don't actually write MaxMessageSize bytes — just verify the length check passes.
	// The read will fail at io.ReadFull because the buffer is empty, but it should NOT
	// fail at the size check.
	buf := new(bytes.Buffer)
	buf.WriteByte('Q')
	binary.Write(buf, binary.BigEndian, int32(MaxMessageSize+4)) // payload = MaxMessageSize

	_, err := ReadMessage(buf)
	if err == nil {
		t.Fatal("expected error (incomplete read), got nil")
	}
	// Should fail at read, not at size check
	if strings.Contains(err.Error(), "message too large") {
		t.Errorf("MaxMessageSize payload should be accepted, got: %v", err)
	}
}

func TestReadMessage_JustOverMaxSize(t *testing.T) {
	buf := new(bytes.Buffer)
	buf.WriteByte('Q')
	binary.Write(buf, binary.BigEndian, int32(MaxMessageSize+5)) // payload = MaxMessageSize+1

	_, err := ReadMessage(buf)
	if err == nil {
		t.Fatal("expected error for over-max message, got nil")
	}
	if !strings.Contains(err.Error(), "message too large") {
		t.Errorf("expected 'message too large' error, got: %v", err)
	}
}
