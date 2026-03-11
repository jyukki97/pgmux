package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// PG wire protocol message types
const (
	// Frontend (client → server)
	MsgQuery       byte = 'Q'
	MsgTerminate   byte = 'X'
	MsgParse       byte = 'P'
	MsgBind        byte = 'B'
	MsgDescribe    byte = 'D'
	MsgExecute     byte = 'E'
	MsgSync        byte = 'S'
	MsgClose       byte = 'C'

	// Backend (server → client)
	MsgAuthentication byte = 'R'
	MsgParameterStatus byte = 'S'
	MsgBackendKeyData  byte = 'K'
	MsgReadyForQuery   byte = 'Z'
	MsgRowDescription  byte = 'T'
	MsgDataRow         byte = 'D'
	MsgCommandComplete byte = 'C'
	MsgErrorResponse   byte = 'E'
	MsgNoticeResponse  byte = 'N'
)

// SSLRequestCode is the magic number for SSL negotiation
const SSLRequestCode = 80877103

// MaxMessageSize is the maximum allowed payload size for a single PG message (16 MB).
// Prevents OOM from malicious length headers.
const MaxMessageSize = 16 * 1024 * 1024

// Message represents a PG wire protocol message.
// For startup messages, Type is 0.
type Message struct {
	Type    byte
	Payload []byte
}

// ReadStartupMessage reads the initial startup message from a client.
// The startup message has no type byte — just length + payload.
func ReadStartupMessage(r io.Reader) (*Message, error) {
	var length int32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read startup length: %w", err)
	}

	if length < 4 || length > 10000 {
		return nil, fmt.Errorf("invalid startup message length: %d", length)
	}

	payload := make([]byte, length-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read startup payload: %w", err)
	}

	return &Message{Type: 0, Payload: payload}, nil
}

// ReadMessage reads a typed PG message (1 byte type + 4 byte length + payload).
func ReadMessage(r io.Reader) (*Message, error) {
	var typeBuf [1]byte
	if _, err := io.ReadFull(r, typeBuf[:]); err != nil {
		return nil, fmt.Errorf("read message type: %w", err)
	}

	var length int32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read message length: %w", err)
	}

	if length < 4 {
		return nil, fmt.Errorf("invalid message length: %d", length)
	}

	payloadLen := int(length - 4)
	if payloadLen > MaxMessageSize {
		return nil, fmt.Errorf("message too large: %d bytes (max %d)", payloadLen, MaxMessageSize)
	}

	payload := make([]byte, payloadLen)
	if len(payload) > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("read message payload: %w", err)
		}
	}

	return &Message{Type: typeBuf[0], Payload: payload}, nil
}

// WriteMessage writes a typed PG message to the writer.
func WriteMessage(w io.Writer, msgType byte, payload []byte) error {
	buf := make([]byte, 1+4+len(payload))
	buf[0] = msgType
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)

	_, err := w.Write(buf)
	return err
}

// WriteRaw writes raw bytes (for startup messages that have no type byte).
func WriteRaw(w io.Writer, data []byte) error {
	_, err := w.Write(data)
	return err
}

// BuildStartupMessage builds a startup message from parameters.
func BuildStartupMessage(params map[string]string) []byte {
	// Protocol version 3.0
	var buf []byte
	buf = binary.BigEndian.AppendUint32(buf, 0) // placeholder for length
	buf = binary.BigEndian.AppendUint16(buf, 3)  // major version
	buf = binary.BigEndian.AppendUint16(buf, 0)  // minor version

	for k, v := range params {
		buf = append(buf, []byte(k)...)
		buf = append(buf, 0)
		buf = append(buf, []byte(v)...)
		buf = append(buf, 0)
	}
	buf = append(buf, 0) // terminator

	binary.BigEndian.PutUint32(buf[0:4], uint32(len(buf)))
	return buf
}

// ParseStartupParams extracts key-value parameters from a startup message payload.
// Payload starts with 4 bytes of protocol version, then null-terminated key-value pairs.
func ParseStartupParams(payload []byte) (major, minor uint16, params map[string]string) {
	if len(payload) < 4 {
		return 0, 0, nil
	}

	major = binary.BigEndian.Uint16(payload[0:2])
	minor = binary.BigEndian.Uint16(payload[2:4])
	params = make(map[string]string)

	rest := payload[4:]
	for len(rest) > 1 { // need at least 1 byte key + null terminator
		keyEnd := indexOf(rest, 0)
		if keyEnd < 0 {
			break
		}
		key := string(rest[:keyEnd])
		rest = rest[keyEnd+1:]

		valEnd := indexOf(rest, 0)
		if valEnd < 0 {
			break
		}
		val := string(rest[:valEnd])
		rest = rest[valEnd+1:]

		params[key] = val
	}

	return major, minor, params
}

// ExtractQueryText extracts the SQL string from a Query message payload.
// Query payload is a null-terminated string.
func ExtractQueryText(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	// Remove trailing null terminator
	end := indexOf(payload, 0)
	if end >= 0 {
		return string(payload[:end])
	}
	return string(payload)
}

// ParseParseMessage extracts the statement name and query text from a Parse ('P') message payload.
// Parse payload format: statement_name (string\0) + query (string\0) + int16 param_count + param OIDs
func ParseParseMessage(payload []byte) (stmtName, query string) {
	nameEnd := indexOf(payload, 0)
	if nameEnd < 0 {
		return "", ""
	}
	stmtName = string(payload[:nameEnd])

	rest := payload[nameEnd+1:]
	queryEnd := indexOf(rest, 0)
	if queryEnd < 0 {
		return stmtName, ""
	}
	query = string(rest[:queryEnd])
	return stmtName, query
}

// ParseBindMessage extracts the destination portal and source statement name from a Bind ('B') message.
// Bind payload format: portal_name (string\0) + statement_name (string\0) + ...
func ParseBindMessage(payload []byte) (portal, stmtName string) {
	portalEnd := indexOf(payload, 0)
	if portalEnd < 0 {
		return "", ""
	}
	portal = string(payload[:portalEnd])

	rest := payload[portalEnd+1:]
	nameEnd := indexOf(rest, 0)
	if nameEnd < 0 {
		return portal, ""
	}
	stmtName = string(rest[:nameEnd])
	return portal, stmtName
}

// ParseCloseMessage extracts the type ('S' for statement, 'P' for portal) and name from a Close ('C') message.
// Close payload format: type_byte + name (string\0)
func ParseCloseMessage(payload []byte) (closeType byte, name string) {
	if len(payload) < 2 {
		return 0, ""
	}
	closeType = payload[0]
	nameEnd := indexOf(payload[1:], 0)
	if nameEnd < 0 {
		return closeType, ""
	}
	return closeType, string(payload[1 : 1+nameEnd])
}

func indexOf(data []byte, b byte) int {
	for i, v := range data {
		if v == b {
			return i
		}
	}
	return -1
}
