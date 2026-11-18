// Package opvarsmodel owns the shared operator-variable bounds used by the
// service, typed client, CLI and MCP adapter. Values are opaque strings; no
// component interprets them.
package opvarsmodel

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxKeyBytes bounds a variable name.
	MaxKeyBytes = 64
	// MaxValueBytes bounds a variable value.
	MaxValueBytes = 240
	// MaxKeys bounds the variables set in one scope (the instance, or one project's overrides).
	MaxKeys = 32

	ScopeInstance = "instance"
	ScopeProject  = "project"
)

// ValidKey admits 1-64 bytes of lowercase ASCII letters, digits, '_', '.' and
// '-', starting with a letter or digit.
func ValidKey(key string) bool {
	if key == "" || len(key) > MaxKeyBytes {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '.' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidValue admits 1-240 bytes of valid UTF-8 without control characters or
// surrounding whitespace.
func ValidValue(value string) bool {
	return value != "" && len(value) <= MaxValueBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

// ValidScope admits the two storage scopes.
func ValidScope(scope string) bool { return scope == ScopeInstance || scope == ScopeProject }
