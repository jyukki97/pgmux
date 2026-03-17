package proxy

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/jyukki97/pgmux/internal/protocol"
)

// PreparedStmt holds metadata for a registered prepared statement.
type PreparedStmt struct {
	Query     string
	ParamOIDs []uint32
}

// maxSynthStatements is the maximum number of prepared statements a single
// client session may register before the oldest is evicted. This prevents
// memory exhaustion from clients that send infinite Parse messages without Close.
const maxSynthStatements = 10000

// Synthesizer manages prepared statements and synthesizes Simple Queries
// from Parse+Bind parameter data in multiplex mode.
type Synthesizer struct {
	mu         sync.Mutex
	statements map[string]*PreparedStmt // name → statement
	order      []string                 // insertion order for LRU eviction
}

// NewSynthesizer creates a new Synthesizer.
func NewSynthesizer() *Synthesizer {
	return &Synthesizer{
		statements: make(map[string]*PreparedStmt),
	}
}

// RegisterStatement records a prepared statement's query and parameter OIDs.
func (s *Synthesizer) RegisterStatement(name, query string, paramOIDs []uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If already registered, just update in place
	if _, ok := s.statements[name]; ok {
		s.statements[name] = &PreparedStmt{Query: query, ParamOIDs: paramOIDs}
		return
	}

	// Evict oldest if at capacity
	if len(s.statements) >= maxSynthStatements {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.statements, oldest)
	}

	s.statements[name] = &PreparedStmt{
		Query:     query,
		ParamOIDs: paramOIDs,
	}
	s.order = append(s.order, name)
}

// GetStatement returns the registered statement, or nil if not found.
func (s *Synthesizer) GetStatement(name string) *PreparedStmt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statements[name]
}

// CloseStatement removes a prepared statement from the registry.
func (s *Synthesizer) CloseStatement(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.statements, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Synthesize builds a Simple Query string by replacing $N placeholders
// with literal values from the Bind parameters.
func (s *Synthesizer) Synthesize(stmtName string, params [][]byte, formatCodes []int16) (string, error) {
	s.mu.Lock()
	stmt, ok := s.statements[stmtName]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown statement: %q", stmtName)
	}

	query := stmt.Query
	paramOIDs := stmt.ParamOIDs

	// Build literal values for each parameter
	literals := make([]string, len(params))
	for i, param := range params {
		var oid uint32
		if i < len(paramOIDs) {
			oid = paramOIDs[i]
		}
		var fc int16
		if len(formatCodes) == 1 {
			fc = formatCodes[0] // all params use same format
		} else if i < len(formatCodes) {
			fc = formatCodes[i]
		}
		lit, err := protocol.ParamToLiteral(param, oid, fc)
		if err != nil {
			return "", fmt.Errorf("param $%d: %w", i+1, err)
		}
		literals[i] = lit
	}

	// Replace placeholders $1, $2, ... with literal values
	return replacePlaceholders(query, literals)
}

// replacePlaceholders replaces $N placeholders in the query with literal values.
// Placeholders inside string literals (single-quoted) are NOT replaced.
func replacePlaceholders(query string, literals []string) (string, error) {
	var result strings.Builder
	result.Grow(len(query) + len(query)/2) // rough estimate

	i := 0
	for i < len(query) {
		ch := query[i]

		// Skip single-quoted string literals
		if ch == '\'' {
			result.WriteByte(ch)
			i++
			for i < len(query) {
				if query[i] == '\'' {
					result.WriteByte('\'')
					i++
					// Escaped quote '' — continue in string
					if i < len(query) && query[i] == '\'' {
						result.WriteByte('\'')
						i++
						continue
					}
					break // end of string literal
				}
				result.WriteByte(query[i])
				i++
			}
			continue
		}

		// Skip dollar-quoted string literals ($$...$$, $tag$...$tag$)
		if ch == '$' && i+1 < len(query) {
			tag, tagEnd := parseDollarTag(query, i)
			if tagEnd > i {
				// Write opening tag
				result.WriteString(tag)
				j := tagEnd
				// Find closing tag
				for j < len(query) {
					closeIdx := strings.Index(query[j:], tag)
					if closeIdx < 0 {
						// No closing tag found — write rest as-is
						result.WriteString(query[j:])
						return result.String(), nil
					}
					result.WriteString(query[j : j+closeIdx+len(tag)])
					i = j + closeIdx + len(tag)
					goto nextChar
				}
			}

			// Check for placeholder $N
			if query[i+1] >= '1' && query[i+1] <= '9' {
				numStart := i + 1
				numEnd := numStart
				for numEnd < len(query) && query[numEnd] >= '0' && query[numEnd] <= '9' {
					numEnd++
				}
				idx, err := strconv.Atoi(query[numStart:numEnd])
				if err == nil && idx >= 1 && idx <= len(literals) {
					result.WriteString(literals[idx-1])
					i = numEnd
					continue
				}
			}
		}

		result.WriteByte(ch)
		i++
		continue

	nextChar:
		continue
	}

	return result.String(), nil
}

// parseDollarTag checks if position i starts a dollar-quoted tag ($$ or $tag$).
// Returns the tag string and the position after the tag, or (_, i) if not a tag.
func parseDollarTag(query string, i int) (string, int) {
	if i >= len(query) || query[i] != '$' {
		return "", i
	}
	j := i + 1
	// $$ case
	if j < len(query) && query[j] == '$' {
		return "$$", j + 1
	}
	// $tag$ case: tag must be [a-zA-Z_][a-zA-Z0-9_]*
	if j >= len(query) || !isTagStart(query[j]) {
		return "", i
	}
	for j < len(query) && isTagChar(query[j]) {
		j++
	}
	if j < len(query) && query[j] == '$' {
		tag := query[i : j+1]
		return tag, j + 1
	}
	return "", i
}

func isTagStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isTagChar(c byte) bool {
	return isTagStart(c) || (c >= '0' && c <= '9')
}
