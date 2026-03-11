package router

import (
	"testing"
)

func TestParseLSN(t *testing.T) {
	tests := []struct {
		input   string
		want    LSN
		wantErr bool
	}{
		{"0/16B3748", 0x016B3748, false},
		{"0/0", 0, false},
		{"1/0", 0x100000000, false},
		{"FF/FFFFFFFF", 0xFFFFFFFFFF, false},
		{"A/B", 0xA0000000B, false},
		{"0/1", 1, false},
		// invalid cases
		{"", 0, true},
		{"noslash", 0, true},
		{"0/GGG", 0, true},
		{"ZZZ/0", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseLSN(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseLSN(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseLSN(%q) = %d (0x%X), want %d (0x%X)", tt.input, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestLSNString(t *testing.T) {
	tests := []struct {
		lsn  LSN
		want string
	}{
		{0x016B3748, "0/16B3748"},
		{0, "0/0"},
		{0x100000000, "1/0"},
		{0xFFFFFFFFFF, "FF/FFFFFFFF"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.lsn.String()
			if got != tt.want {
				t.Errorf("LSN(%d).String() = %q, want %q", tt.lsn, got, tt.want)
			}
		})
	}
}

func TestLSNRoundTrip(t *testing.T) {
	inputs := []string{"0/16B3748", "0/0", "1/0", "FF/FFFFFFFF", "A/B"}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			lsn, err := ParseLSN(input)
			if err != nil {
				t.Fatalf("ParseLSN(%q) unexpected error: %v", input, err)
			}
			got := lsn.String()
			if got != input {
				t.Errorf("round-trip failed: %q → %d → %q", input, lsn, got)
			}
		})
	}
}

func TestLSNComparison(t *testing.T) {
	a, _ := ParseLSN("0/16B3748")
	b, _ := ParseLSN("0/16B3749")
	c, _ := ParseLSN("1/0")

	if a >= b {
		t.Errorf("expected %v < %v", a, b)
	}
	if b >= c {
		t.Errorf("expected %v < %v", b, c)
	}
	if a >= c {
		t.Errorf("expected %v < %v", a, c)
	}

	// equal
	d, _ := ParseLSN("0/16B3748")
	if a != d {
		t.Errorf("expected %v == %v", a, d)
	}
}

func TestLSNIsZero(t *testing.T) {
	if !InvalidLSN.IsZero() {
		t.Error("InvalidLSN should be zero")
	}
	lsn, _ := ParseLSN("0/1")
	if lsn.IsZero() {
		t.Error("non-zero LSN should not be zero")
	}
}
