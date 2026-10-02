// Package dotenv reads application ".env" files (Docker Compose style)
// so Ballast can use an app's own database credentials instead of a
// pasted copy (spec §18.2). Values are read at job-run time and are
// never stored.
package dotenv

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Parse reads dotenv text: KEY=value, "double" and 'single' quoted
// values, # comments, blank lines, an optional "export " prefix, empty
// values and CRLF line endings. Double-quoted values understand \n, \r,
// \t, \" and \\ escapes; single-quoted values are literal. Unquoted
// values are trimmed and end at an unquoted " #" comment. The last
// definition of a key wins.
func Parse(data []byte) (map[string]string, error) {
	out := map[string]string{}
	text := strings.TrimPrefix(string(data), "\ufeff")
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d is not in KEY=value form", i+1)
		}
		val, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %w", i+1, key, err)
		}
		out[key] = val
	}
	return out, nil
}

func parseValue(raw string) (string, error) {
	switch {
	case strings.HasPrefix(raw, "'"):
		end := strings.Index(raw[1:], "'")
		if end < 0 {
			return "", fmt.Errorf("unterminated single quote")
		}
		return raw[1 : 1+end], nil
	case strings.HasPrefix(raw, `"`):
		var b strings.Builder
		for i := 1; i < len(raw); i++ {
			switch c := raw[i]; c {
			case '"':
				return b.String(), nil
			case '\\':
				i++
				if i >= len(raw) {
					return "", fmt.Errorf("unterminated double quote")
				}
				switch raw[i] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '"', '\\':
					b.WriteByte(raw[i])
				default:
					b.WriteByte('\\')
					b.WriteByte(raw[i])
				}
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("unterminated double quote")
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw), nil
}

// ReadFile parses the .env file at path. Errors name the file but never
// include any value.
func ReadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Lookup returns the value of key in the .env file at path. A missing
// file or key is an error naming both; an empty value is also an error,
// so a blank password can never be used silently.
func Lookup(path, key string) (string, error) {
	m, err := ReadFile(path)
	if err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("%s has no %s entry", path, key)
	}
	if v == "" {
		return "", fmt.Errorf("%s has %s but its value is empty", path, key)
	}
	return v, nil
}

// Keys lists the key names (never values) found in the .env file at
// path, sorted, for the "test this source" check.
func Keys(path string) ([]string, error) {
	m, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}
