package proxy

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/jyukki97/pgmux/internal/protocol"
)

// buildParsePayload constructs a Parse message payload: stmtName\0 + query\0 + int16(0)
func buildParsePayload(stmtName, query string) []byte {
	var buf []byte
	buf = append(buf, []byte(stmtName)...)
	buf = append(buf, 0)
	buf = append(buf, []byte(query)...)
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 0) // no param type hints
	return buf
}

// buildBindPayload constructs a Bind message payload with result format codes.
func buildBindPayload(portal, stmtName string, resultFormatCodes []int16) []byte {
	var buf []byte
	buf = append(buf, []byte(portal)...)
	buf = append(buf, 0)
	buf = append(buf, []byte(stmtName)...)
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 0) // 0 parameter format codes
	buf = binary.BigEndian.AppendUint16(buf, 0) // 0 parameters
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(resultFormatCodes)))
	for _, fc := range resultFormatCodes {
		buf = binary.BigEndian.AppendUint16(buf, uint16(fc))
	}
	return buf
}

// buildExecutePayload constructs an Execute message payload: portal\0 + int32(maxRows).
func buildExecutePayload(portal string, maxRows uint32) []byte {
	var buf bytes.Buffer
	buf.WriteString(portal)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.BigEndian, maxRows)
	return buf.Bytes()
}

func TestHasParameterPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		buf  []*protocol.Message
		want bool
	}{
		{
			name: "no placeholders",
			buf: []*protocol.Message{
				{Type: protocol.MsgParse, Payload: buildParsePayload("", "SELECT 1")},
			},
			want: false,
		},
		{
			name: "has $1 placeholder",
			buf: []*protocol.Message{
				{Type: protocol.MsgParse, Payload: buildParsePayload("", "SELECT * FROM t WHERE id = $1")},
			},
			want: true,
		},
		{
			name: "has $9 placeholder",
			buf: []*protocol.Message{
				{Type: protocol.MsgParse, Payload: buildParsePayload("", "INSERT INTO t VALUES ($9)")},
			},
			want: true,
		},
		{
			name: "dollar but not placeholder ($0)",
			buf: []*protocol.Message{
				{Type: protocol.MsgParse, Payload: buildParsePayload("", "SELECT '$0'")},
			},
			want: false,
		},
		{
			name: "dollar at end of query",
			buf: []*protocol.Message{
				{Type: protocol.MsgParse, Payload: buildParsePayload("", "SELECT $")},
			},
			want: false,
		},
		{
			name: "empty buf",
			buf:  nil,
			want: false,
		},
		{
			name: "non-parse message",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: []byte{0}},
			},
			want: false,
		},
		{
			name: "multiple messages, second has placeholder",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: []byte{0}},
				{Type: protocol.MsgParse, Payload: buildParsePayload("s1", "SELECT $3")},
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasParameterPlaceholders(tc.buf)
			if got != tc.want {
				t.Errorf("hasParameterPlaceholders() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasBinaryFormatOrPartialFetch(t *testing.T) {
	tests := []struct {
		name string
		buf  []*protocol.Message
		want bool
	}{
		{
			name: "empty buf",
			buf:  nil,
			want: false,
		},
		{
			name: "text format only",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: buildBindPayload("", "s1", []int16{0})},
			},
			want: false,
		},
		{
			name: "binary format code",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: buildBindPayload("", "s1", []int16{1})},
			},
			want: true,
		},
		{
			name: "mixed format codes",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: buildBindPayload("", "s1", []int16{0, 1})},
			},
			want: true,
		},
		{
			name: "no result format codes (all text default)",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: buildBindPayload("", "s1", nil)},
			},
			want: false,
		},
		{
			name: "execute maxRows=0 (fetch all)",
			buf: []*protocol.Message{
				{Type: protocol.MsgExecute, Payload: buildExecutePayload("", 0)},
			},
			want: false,
		},
		{
			name: "execute maxRows=100 (partial fetch)",
			buf: []*protocol.Message{
				{Type: protocol.MsgExecute, Payload: buildExecutePayload("", 100)},
			},
			want: true,
		},
		{
			name: "parse message only (no bind/execute)",
			buf: []*protocol.Message{
				{Type: protocol.MsgParse, Payload: buildParsePayload("", "SELECT 1")},
			},
			want: false,
		},
		{
			name: "malformed bind payload",
			buf: []*protocol.Message{
				{Type: protocol.MsgBind, Payload: []byte{0xFF}},
			},
			want: true, // can't parse → skip cache to be safe
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasBinaryFormatOrPartialFetch(tc.buf)
			if got != tc.want {
				t.Errorf("hasBinaryFormatOrPartialFetch() = %v, want %v", got, tc.want)
			}
		})
	}
}
