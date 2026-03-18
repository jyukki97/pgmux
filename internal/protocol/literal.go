package protocol

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// PostgreSQL type OIDs
const (
	OIDBoolean      uint32 = 16
	OIDInt2         uint32 = 21
	OIDInt4         uint32 = 23
	OIDInt8         uint32 = 20
	OIDFloat4       uint32 = 700
	OIDFloat8       uint32 = 701
	OIDNumeric      uint32 = 1700
	OIDText         uint32 = 25
	OIDVarchar      uint32 = 1043
	OIDBytea        uint32 = 17
	OIDTimestamp    uint32 = 1114
	OIDTimestampTZ  uint32 = 1184
	OIDDate         uint32 = 1082
	OIDUUID         uint32 = 2950
	OIDJSON         uint32 = 114
	OIDJSONB        uint32 = 3802
	OIDChar         uint32 = 18
	OIDBpchar       uint32 = 1042
	OIDName         uint32 = 19
	OIDOid          uint32 = 26
	OIDTime         uint32 = 1083
	OIDTimeTZ       uint32 = 1266
	OIDInterval     uint32 = 1186
	OIDInet         uint32 = 869
	OIDCidr         uint32 = 650
	OIDMacaddr      uint32 = 829
	OIDPoint        uint32 = 600
	OIDBoolArray    uint32 = 1000
	OIDInt2Array    uint32 = 1005
	OIDInt4Array    uint32 = 1007
	OIDInt8Array    uint32 = 1016
	OIDFloat4Array  uint32 = 1021
	OIDFloat8Array  uint32 = 1022
	OIDTextArray    uint32 = 1009
	OIDVarcharArray uint32 = 1015
)

// ParamToLiteral converts a parameter value to a safe SQL literal string.
// Returns "NULL" for nil values.
// formatCode: 0=text, 1=binary.
func ParamToLiteral(value []byte, oid uint32, formatCode int16) (string, error) {
	if value == nil {
		return "NULL", nil
	}

	// Binary format parameters — convert based on OID
	if formatCode == 1 {
		return binaryParamToLiteral(value, oid)
	}

	// Text format — the value is the text representation of the type
	text := string(value)
	return textParamToLiteral(text, oid)
}

// textParamToLiteral converts a text-format parameter to SQL literal.
func textParamToLiteral(text string, oid uint32) (string, error) {
	switch oid {
	case OIDBoolean:
		switch strings.ToLower(text) {
		case "t", "true", "1", "yes", "on":
			return "TRUE", nil
		case "f", "false", "0", "no", "off":
			return "FALSE", nil
		default:
			return "", fmt.Errorf("invalid boolean value: %q", text)
		}

	case OIDInt2, OIDInt4, OIDInt8, OIDOid:
		// Validate it's actually a number to prevent injection
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return "", fmt.Errorf("invalid integer value: %q", text)
		}
		return text, nil

	case OIDFloat4, OIDFloat8:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return "", fmt.Errorf("invalid float value: %q", text)
		}
		if math.IsNaN(f) {
			return "'NaN'::float8", nil
		}
		if math.IsInf(f, 1) {
			return "'Infinity'::float8", nil
		}
		if math.IsInf(f, -1) {
			return "'-Infinity'::float8", nil
		}
		return text, nil

	case OIDNumeric:
		// Numeric can be very large; validate format: optional sign, digits, optional decimal point + digits
		if !isValidNumeric(text) {
			return "", fmt.Errorf("invalid numeric value: %q", text)
		}
		return text, nil

	case OIDBytea:
		return "E'\\\\x" + hex.EncodeToString(value(text)) + "'", nil

	case OIDUUID:
		if !isValidUUID(text) {
			return "", fmt.Errorf("invalid UUID value: %q", text)
		}
		return escapeStringLiteral(text), nil

	case OIDJSON, OIDJSONB:
		return escapeStringLiteral(text), nil

	case OIDText, OIDVarchar, OIDChar, OIDBpchar, OIDName:
		return escapeStringLiteral(text), nil

	case OIDTimestamp, OIDTimestampTZ, OIDDate, OIDTime, OIDTimeTZ, OIDInterval:
		return escapeStringLiteral(text), nil

	case OIDInet, OIDCidr, OIDMacaddr:
		return escapeStringLiteral(text), nil

	case OIDPoint:
		return escapeStringLiteral(text), nil

	case OIDBoolArray, OIDInt2Array, OIDInt4Array, OIDInt8Array,
		OIDFloat4Array, OIDFloat8Array, OIDTextArray, OIDVarcharArray:
		return escapeStringLiteral(text), nil

	default:
		// Unknown type: treat as text with escaping (safe default)
		return escapeStringLiteral(text), nil
	}
}

// binaryParamToLiteral converts a binary-format parameter to SQL literal.
func binaryParamToLiteral(data []byte, oid uint32) (string, error) {
	switch oid {
	case OIDBoolean:
		if len(data) != 1 {
			return "", fmt.Errorf("invalid boolean binary length: %d", len(data))
		}
		if data[0] != 0 {
			return "TRUE", nil
		}
		return "FALSE", nil

	case OIDInt2:
		if len(data) != 2 {
			return "", fmt.Errorf("invalid int2 binary length: %d", len(data))
		}
		v := int16(data[0])<<8 | int16(data[1])
		return strconv.FormatInt(int64(v), 10), nil

	case OIDInt4:
		if len(data) != 4 {
			return "", fmt.Errorf("invalid int4 binary length: %d", len(data))
		}
		v := int32(data[0])<<24 | int32(data[1])<<16 | int32(data[2])<<8 | int32(data[3])
		return strconv.FormatInt(int64(v), 10), nil

	case OIDInt8:
		if len(data) != 8 {
			return "", fmt.Errorf("invalid int8 binary length: %d", len(data))
		}
		var v int64
		for i := 0; i < 8; i++ {
			v = v<<8 | int64(data[i])
		}
		return strconv.FormatInt(v, 10), nil

	case OIDFloat4:
		if len(data) != 4 {
			return "", fmt.Errorf("invalid float4 binary length: %d", len(data))
		}
		bits := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
		f := math.Float32frombits(bits)
		if math.IsNaN(float64(f)) {
			return "'NaN'::float4", nil
		}
		if math.IsInf(float64(f), 1) {
			return "'Infinity'::float4", nil
		}
		if math.IsInf(float64(f), -1) {
			return "'-Infinity'::float4", nil
		}
		return strconv.FormatFloat(float64(f), 'f', -1, 32), nil

	case OIDFloat8:
		if len(data) != 8 {
			return "", fmt.Errorf("invalid float8 binary length: %d", len(data))
		}
		var bits uint64
		for i := 0; i < 8; i++ {
			bits = bits<<8 | uint64(data[i])
		}
		f := math.Float64frombits(bits)
		if math.IsNaN(f) {
			return "'NaN'::float8", nil
		}
		if math.IsInf(f, 1) {
			return "'Infinity'::float8", nil
		}
		if math.IsInf(f, -1) {
			return "'-Infinity'::float8", nil
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil

	case OIDBytea:
		return "E'\\\\x" + hex.EncodeToString(data) + "'", nil

	default:
		// For other types in binary format, treat as text if printable, else bytea
		return escapeStringLiteral(string(data)), nil
	}
}

// escapeStringLiteral returns a safely escaped PostgreSQL string literal.
// Uses standard_conforming_strings=on (the default since PG 9.1).
func escapeStringLiteral(s string) string {
	// Use dollar quoting if the string contains both single quotes and backslashes
	// to avoid complex escaping. We use a tag that won't appear in the string.
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' {
			b.WriteString("''")
		} else if ch == 0 {
			// NULL bytes are not allowed in SQL string literals — skip them
			continue
		} else {
			b.WriteByte(ch)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// isValidNumeric checks if a string is a valid PostgreSQL numeric literal.
func isValidNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	// Handle special values
	if strings.EqualFold(s, "NaN") || strings.EqualFold(s, "Infinity") || strings.EqualFold(s, "-Infinity") {
		return true
	}
	i := 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return false
	}
	hasDigit := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		hasDigit = true
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			hasDigit = true
			i++
		}
	}
	// Optional exponent
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return false
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	return hasDigit && i == len(s)
}

// isValidUUID checks if a string is a valid UUID format.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// value is a helper that returns the raw bytes from a text representation.
func value(text string) []byte {
	return []byte(text)
}
