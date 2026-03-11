package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestReadStartupMessage(t *testing.T) {
	params := map[string]string{
		"user":     "postgres",
		"database": "testdb",
	}
	raw := BuildStartupMessage(params)

	msg, err := ReadStartupMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStartupMessage() error: %v", err)
	}

	major, minor, parsed := ParseStartupParams(msg.Payload)
	if major != 3 || minor != 0 {
		t.Errorf("version = %d.%d, want 3.0", major, minor)
	}
	if parsed["user"] != "postgres" {
		t.Errorf("user = %q, want %q", parsed["user"], "postgres")
	}
	if parsed["database"] != "testdb" {
		t.Errorf("database = %q, want %q", parsed["database"], "testdb")
	}
}

func TestReadMessage(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("SELECT 1\x00")
	WriteMessage(&buf, MsgQuery, payload)

	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage() error: %v", err)
	}
	if msg.Type != MsgQuery {
		t.Errorf("Type = %c, want %c", msg.Type, MsgQuery)
	}
	if !bytes.Equal(msg.Payload, payload) {
		t.Errorf("Payload = %q, want %q", msg.Payload, payload)
	}
}

func TestWriteMessage(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{0, 0, 0, 0} // AuthenticationOk
	err := WriteMessage(&buf, MsgAuthentication, payload)
	if err != nil {
		t.Fatalf("WriteMessage() error: %v", err)
	}

	data := buf.Bytes()
	if data[0] != MsgAuthentication {
		t.Errorf("type byte = %c, want %c", data[0], MsgAuthentication)
	}
	length := binary.BigEndian.Uint32(data[1:5])
	if length != uint32(4+len(payload)) {
		t.Errorf("length = %d, want %d", length, 4+len(payload))
	}
}

func TestReadStartupMessage_SSLRequest(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, int32(8))
	binary.Write(&buf, binary.BigEndian, int32(SSLRequestCode))

	msg, err := ReadStartupMessage(&buf)
	if err != nil {
		t.Fatalf("ReadStartupMessage() error: %v", err)
	}

	code := binary.BigEndian.Uint32(msg.Payload[0:4])
	if code != SSLRequestCode {
		t.Errorf("code = %d, want %d", code, SSLRequestCode)
	}
}

func TestExtractQueryText(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"normal", []byte("SELECT 1\x00"), "SELECT 1"},
		{"no null", []byte("SELECT 1"), "SELECT 1"},
		{"empty", []byte{}, ""},
		{"just null", []byte{0}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractQueryText(tt.payload)
			if got != tt.want {
				t.Errorf("ExtractQueryText() = %q, want %q", got, tt.want)
			}
		})
	}
}
