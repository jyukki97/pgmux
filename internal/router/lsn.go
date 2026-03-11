package router

import (
	"fmt"
	"strconv"
	"strings"
)

// LSN represents a PostgreSQL Log Sequence Number as a uint64.
type LSN uint64

// InvalidLSN is the zero value indicating no LSN has been recorded.
const InvalidLSN LSN = 0

// ParseLSN parses a PostgreSQL LSN string (e.g. "0/16B3748") into an LSN value.
func ParseLSN(s string) (LSN, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid LSN format: %q", s)
	}

	hi, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN high part: %w", err)
	}

	lo, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN low part: %w", err)
	}

	return LSN(hi<<32 | lo), nil
}

// String formats the LSN back to PostgreSQL format (e.g. "0/16B3748").
func (l LSN) String() string {
	return fmt.Sprintf("%X/%X", uint32(l>>32), uint32(l))
}

// IsZero returns true if the LSN is the zero/invalid value.
func (l LSN) IsZero() bool {
	return l == InvalidLSN
}
