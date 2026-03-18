package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestParseServerFirst(t *testing.T) {
	salt := base64.StdEncoding.EncodeToString([]byte("somesalt1234"))
	tests := []struct {
		name      string
		msg       string
		wantNonce string
		wantSalt  string
		wantIter  int
		wantErr   bool
	}{
		{
			name:      "valid message",
			msg:       "r=clientNonce+serverPart,s=" + salt + ",i=4096",
			wantNonce: "clientNonce+serverPart",
			wantSalt:  "somesalt1234",
			wantIter:  4096,
		},
		{
			name:    "missing nonce",
			msg:     "s=" + salt + ",i=4096",
			wantErr: true,
		},
		{
			name:    "missing salt",
			msg:     "r=nonce123,i=4096",
			wantErr: true,
		},
		{
			name:    "missing iterations",
			msg:     "r=nonce123,s=" + salt,
			wantErr: true,
		},
		{
			name:    "empty message",
			msg:     "",
			wantErr: true,
		},
		{
			name:    "invalid salt encoding",
			msg:     "r=nonce,s=!!!invalid!!!,i=4096",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nonce, saltBytes, iter, err := parseServerFirst(tc.msg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if nonce != tc.wantNonce {
				t.Errorf("nonce = %q, want %q", nonce, tc.wantNonce)
			}
			if string(saltBytes) != tc.wantSalt {
				t.Errorf("salt = %q, want %q", string(saltBytes), tc.wantSalt)
			}
			if iter != tc.wantIter {
				t.Errorf("iterations = %d, want %d", iter, tc.wantIter)
			}
		})
	}
}

func TestXorBytes(t *testing.T) {
	tests := []struct {
		name string
		a, b []byte
		want []byte
	}{
		{
			name: "basic xor",
			a:    []byte{0xFF, 0x00, 0xAA},
			b:    []byte{0x0F, 0xF0, 0x55},
			want: []byte{0xF0, 0xF0, 0xFF},
		},
		{
			name: "same bytes",
			a:    []byte{0xAB, 0xCD},
			b:    []byte{0xAB, 0xCD},
			want: []byte{0x00, 0x00},
		},
		{
			name: "zeros",
			a:    []byte{0x00, 0x00},
			b:    []byte{0x00, 0x00},
			want: []byte{0x00, 0x00},
		},
		{
			name: "single byte",
			a:    []byte{0x55},
			b:    []byte{0xAA},
			want: []byte{0xFF},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := xorBytes(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("byte[%d] = 0x%02X, want 0x%02X", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHmacSHA256(t *testing.T) {
	key := []byte("secret-key")
	data := []byte("hello world")

	got := hmacSHA256(key, data)

	// Verify against standard library directly
	h := hmac.New(sha256.New, key)
	h.Write(data)
	want := h.Sum(nil)

	if !hmac.Equal(got, want) {
		t.Errorf("hmacSHA256 mismatch")
	}

	// SHA-256 HMAC output is always 32 bytes
	if len(got) != 32 {
		t.Errorf("output length = %d, want 32", len(got))
	}
}

func TestSha256Sum(t *testing.T) {
	data := []byte("test data")

	got := sha256Sum(data)

	want := sha256.Sum256(data)
	if len(got) != 32 {
		t.Fatalf("output length = %d, want 32", len(got))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("byte[%d] = 0x%02X, want 0x%02X", i, got[i], want[i])
		}
	}
}

func TestGenerateNonce(t *testing.T) {
	n1 := generateNonce()
	n2 := generateNonce()

	// Nonces must not be empty
	if n1 == "" {
		t.Fatal("generateNonce returned empty string")
	}

	// Nonces should be unique
	if n1 == n2 {
		t.Error("two consecutive nonces are identical")
	}

	// Must be valid base64
	if _, err := base64.StdEncoding.DecodeString(n1); err != nil {
		t.Errorf("nonce is not valid base64: %v", err)
	}

	// 18 random bytes → 24 base64 chars
	if len(n1) != 24 {
		t.Errorf("nonce length = %d, want 24", len(n1))
	}
}
