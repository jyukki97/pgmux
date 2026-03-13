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
	_ = WriteMessage(&buf, MsgQuery, payload)

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
	_ = binary.Write(&buf, binary.BigEndian, int32(8))
	_ = binary.Write(&buf, binary.BigEndian, int32(SSLRequestCode))

	msg, err := ReadStartupMessage(&buf)
	if err != nil {
		t.Fatalf("ReadStartupMessage() error: %v", err)
	}

	code := binary.BigEndian.Uint32(msg.Payload[0:4])
	if code != SSLRequestCode {
		t.Errorf("code = %d, want %d", code, SSLRequestCode)
	}
}

func TestParseParseMessage(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		wantStmt string
		wantSQL  string
	}{
		{
			"named statement",
			append(append([]byte("stmt1"), 0), append([]byte("SELECT * FROM users"), 0)...),
			"stmt1", "SELECT * FROM users",
		},
		{
			"unnamed statement",
			append([]byte{0}, append([]byte("INSERT INTO orders VALUES ($1)"), 0)...),
			"", "INSERT INTO orders VALUES ($1)",
		},
		{
			"with param OIDs",
			func() []byte {
				b := append([]byte("s1"), 0)
				b = append(b, append([]byte("SELECT 1"), 0)...)
				b = binary.BigEndian.AppendUint16(b, 0) // 0 params
				return b
			}(),
			"s1", "SELECT 1",
		},
		{
			"empty payload",
			[]byte{},
			"", "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, sql := ParseParseMessage(tt.payload)
			if stmt != tt.wantStmt {
				t.Errorf("stmt = %q, want %q", stmt, tt.wantStmt)
			}
			if sql != tt.wantSQL {
				t.Errorf("sql = %q, want %q", sql, tt.wantSQL)
			}
		})
	}
}

func TestParseBindMessage(t *testing.T) {
	// Bind: portal\0 + stmt_name\0 + ...
	payload := append(append([]byte(""), 0), append([]byte("stmt1"), 0)...)
	portal, stmt := ParseBindMessage(payload)
	if portal != "" {
		t.Errorf("portal = %q, want empty", portal)
	}
	if stmt != "stmt1" {
		t.Errorf("stmt = %q, want %q", stmt, "stmt1")
	}
}

func TestParseParseMessageFull(t *testing.T) {
	tests := []struct {
		name      string
		payload   []byte
		wantStmt  string
		wantQuery string
		wantOIDs  []uint32
		wantErr   bool
	}{
		{
			"with param OIDs",
			func() []byte {
				b := append([]byte("s1"), 0)
				b = append(b, append([]byte("SELECT $1, $2"), 0)...)
				b = binary.BigEndian.AppendUint16(b, 2) // 2 params
				b = binary.BigEndian.AppendUint32(b, 23) // int4
				b = binary.BigEndian.AppendUint32(b, 25) // text
				return b
			}(),
			"s1", "SELECT $1, $2", []uint32{23, 25}, false,
		},
		{
			"no params",
			func() []byte {
				b := append([]byte("s2"), 0)
				b = append(b, append([]byte("SELECT 1"), 0)...)
				b = binary.BigEndian.AppendUint16(b, 0)
				return b
			}(),
			"s2", "SELECT 1", []uint32{}, false,
		},
		{
			"unnamed with OIDs",
			func() []byte {
				b := []byte{0} // empty name
				b = append(b, append([]byte("INSERT INTO t VALUES ($1)"), 0)...)
				b = binary.BigEndian.AppendUint16(b, 1)
				b = binary.BigEndian.AppendUint32(b, 23)
				return b
			}(),
			"", "INSERT INTO t VALUES ($1)", []uint32{23}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, query, oids, err := ParseParseMessageFull(tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if stmt != tt.wantStmt {
				t.Errorf("stmt = %q, want %q", stmt, tt.wantStmt)
			}
			if query != tt.wantQuery {
				t.Errorf("query = %q, want %q", query, tt.wantQuery)
			}
			if len(oids) != len(tt.wantOIDs) {
				t.Fatalf("oids len = %d, want %d", len(oids), len(tt.wantOIDs))
			}
			for i, oid := range oids {
				if oid != tt.wantOIDs[i] {
					t.Errorf("oids[%d] = %d, want %d", i, oid, tt.wantOIDs[i])
				}
			}
		})
	}
}

func TestParseBindMessageFull(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		check   func(t *testing.T, d *BindMessageDetail)
		wantErr bool
	}{
		{
			"basic with text params",
			func() []byte {
				var b []byte
				b = append(b, 0)         // portal ""
				b = append(b, []byte("stmt1")...)
				b = append(b, 0)         // stmt name
				b = binary.BigEndian.AppendUint16(b, 1) // 1 format code
				b = binary.BigEndian.AppendUint16(b, 0) // text
				b = binary.BigEndian.AppendUint16(b, 2) // 2 params
				// param 1: "42"
				b = binary.BigEndian.AppendUint32(b, 2)
				b = append(b, []byte("42")...)
				// param 2: "hello"
				b = binary.BigEndian.AppendUint32(b, 5)
				b = append(b, []byte("hello")...)
				b = binary.BigEndian.AppendUint16(b, 0) // 0 result format codes
				return b
			}(),
			func(t *testing.T, d *BindMessageDetail) {
				if d.Portal != "" {
					t.Errorf("portal = %q, want empty", d.Portal)
				}
				if d.StatementName != "stmt1" {
					t.Errorf("stmt = %q, want stmt1", d.StatementName)
				}
				if len(d.FormatCodes) != 1 || d.FormatCodes[0] != 0 {
					t.Errorf("format codes = %v, want [0]", d.FormatCodes)
				}
				if len(d.Parameters) != 2 {
					t.Fatalf("params len = %d, want 2", len(d.Parameters))
				}
				if string(d.Parameters[0]) != "42" {
					t.Errorf("param[0] = %q, want 42", d.Parameters[0])
				}
				if string(d.Parameters[1]) != "hello" {
					t.Errorf("param[1] = %q, want hello", d.Parameters[1])
				}
				if len(d.ResultFormatCodes) != 0 {
					t.Errorf("result format codes = %v, want empty", d.ResultFormatCodes)
				}
			},
			false,
		},
		{
			"NULL parameter",
			func() []byte {
				var b []byte
				b = append(b, 0)         // portal
				b = append(b, 0)         // stmt
				b = binary.BigEndian.AppendUint16(b, 0) // 0 format codes
				b = binary.BigEndian.AppendUint16(b, 1) // 1 param
				// NULL param: length = -1
				b = append(b, 0xFF, 0xFF, 0xFF, 0xFF)
				b = binary.BigEndian.AppendUint16(b, 0) // 0 result format codes
				return b
			}(),
			func(t *testing.T, d *BindMessageDetail) {
				if len(d.Parameters) != 1 {
					t.Fatalf("params len = %d, want 1", len(d.Parameters))
				}
				if d.Parameters[0] != nil {
					t.Errorf("param[0] = %v, want nil (NULL)", d.Parameters[0])
				}
			},
			false,
		},
		{
			"binary format with result codes",
			func() []byte {
				var b []byte
				b = append(b, []byte("p1")...)
				b = append(b, 0)
				b = append(b, []byte("s1")...)
				b = append(b, 0)
				b = binary.BigEndian.AppendUint16(b, 2) // 2 format codes
				b = binary.BigEndian.AppendUint16(b, 0) // text
				b = binary.BigEndian.AppendUint16(b, 1) // binary
				b = binary.BigEndian.AppendUint16(b, 2) // 2 params
				b = binary.BigEndian.AppendUint32(b, 3)
				b = append(b, []byte("abc")...)
				b = binary.BigEndian.AppendUint32(b, 4) // 4 bytes binary
				b = append(b, 0, 0, 0, 42)
				b = binary.BigEndian.AppendUint16(b, 1) // 1 result format code
				b = binary.BigEndian.AppendUint16(b, 1) // binary
				return b
			}(),
			func(t *testing.T, d *BindMessageDetail) {
				if d.Portal != "p1" {
					t.Errorf("portal = %q, want p1", d.Portal)
				}
				if d.StatementName != "s1" {
					t.Errorf("stmt = %q, want s1", d.StatementName)
				}
				if len(d.FormatCodes) != 2 {
					t.Fatalf("format codes len = %d, want 2", len(d.FormatCodes))
				}
				if d.FormatCodes[0] != 0 || d.FormatCodes[1] != 1 {
					t.Errorf("format codes = %v, want [0, 1]", d.FormatCodes)
				}
				if len(d.Parameters) != 2 {
					t.Fatalf("params len = %d, want 2", len(d.Parameters))
				}
				if string(d.Parameters[0]) != "abc" {
					t.Errorf("param[0] = %q, want abc", d.Parameters[0])
				}
				if len(d.Parameters[1]) != 4 || d.Parameters[1][3] != 42 {
					t.Errorf("param[1] = %v, want [0 0 0 42]", d.Parameters[1])
				}
				if len(d.ResultFormatCodes) != 1 || d.ResultFormatCodes[0] != 1 {
					t.Errorf("result format codes = %v, want [1]", d.ResultFormatCodes)
				}
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseBindMessageFull(tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, d)
			}
		})
	}
}

func TestParseCloseMessage(t *testing.T) {
	// Close statement: 'S' + name\0
	payload := append([]byte{'S'}, append([]byte("stmt1"), 0)...)
	closeType, name := ParseCloseMessage(payload)
	if closeType != 'S' {
		t.Errorf("closeType = %c, want S", closeType)
	}
	if name != "stmt1" {
		t.Errorf("name = %q, want %q", name, "stmt1")
	}

	// Close portal: 'P' + name\0
	payload2 := append([]byte{'P'}, append([]byte("p1"), 0)...)
	closeType2, name2 := ParseCloseMessage(payload2)
	if closeType2 != 'P' {
		t.Errorf("closeType = %c, want P", closeType2)
	}
	if name2 != "p1" {
		t.Errorf("name = %q, want %q", name2, "p1")
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
