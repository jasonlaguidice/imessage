package connector

import (
	"strings"
	"unicode"
)

// normalizePhone strips all non-digit characters (except leading +).
func normalizePhone(phone string) string {
	var b strings.Builder
	for i, r := range phone {
		if r == '+' && i == 0 {
			b.WriteRune(r)
		} else if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// phoneSuffixes returns the number and its last 10/7 digits for flexible matching.
func phoneSuffixes(phone string) []string {
	n := normalizePhone(phone)
	if n == "" {
		return nil
	}
	suffixes := []string{n}
	// Strip leading + for matching
	without := strings.TrimPrefix(n, "+")
	if without != n {
		suffixes = append(suffixes, without)
	}
	// Last 10 digits (US number without country code)
	if len(without) > 10 {
		suffixes = append(suffixes, without[len(without)-10:])
	}
	// Last 7 digits (local number)
	if len(without) > 7 {
		suffixes = append(suffixes, without[len(without)-7:])
	}
	return suffixes
}

// stripNonBase64 removes all characters that are not valid in base64 encoding.
// This handles garbage injected by chat UIs (non-breaking spaces, newlines, etc.).
func stripNonBase64(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
