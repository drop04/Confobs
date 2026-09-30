package confobs

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// EnvFile is the parsed content of a .env-style file.
type EnvFile struct {
	Values map[string]string // KEY -> value (the last assignment wins)
	Keys   []string          // distinct keys in the order they first appear
}

// ParseEnvFile reads a KEY=VALUE file. It understands:
//
//   - blank lines and full-line "# comments"
//   - an optional leading "export "
//   - "double" or 'single' quoted values (quotes removed, no escape sequences)
//   - a trailing " # comment" after an unquoted value
//
// Lines without an "=" are ignored.
func ParseEnvFile(path string) (*EnvFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("confobs: reading env file: %w", err)
	}
	defer f.Close()

	ef := &EnvFile{Values: map[string]string{}}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if _, seen := ef.Values[key]; !seen {
			ef.Keys = append(ef.Keys, key)
		}
		ef.Values[key] = parseEnvValue(val)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("confobs: reading env file: %w", err)
	}
	return ef, nil
}

func parseEnvValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : 1+end]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}
