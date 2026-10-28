package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxCheckEnvFileBytes = 1 << 20

// loadCheckEnvFile overlays nonempty process values on the last .env assignments.
// It neither mutates the process environment nor evaluates shell syntax.
func loadCheckEnvFile(getenv func(string) string) (func(string) string, error) {
	file, err := os.Open(".env")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return getenv, nil
		}
		return nil, errors.New("cannot read env file")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCheckEnvFileBytes+1))
	if err != nil {
		return nil, errors.New("cannot read env file")
	}
	if len(raw) > maxCheckEnvFileBytes {
		return nil, errors.New("env file exceeds the 1 MiB size limit")
	}
	values, err := parseCheckEnvFile(raw)
	if err != nil {
		return nil, err
	}
	return func(key string) string {
		if value := getenv(key); value != "" {
			return value
		}
		return values[key]
	}, nil
}

func parseCheckEnvFile(raw []byte) (map[string]string, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	values := make(map[string]string)
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.ContainsAny(line, "\x00\r") {
			return nil, checkEnvLineError(i+1, "unexpected control character")
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export") && len(line) > len("export") && checkEnvSpace(line[len("export")]) {
			line = strings.TrimSpace(line[len("export"):])
		}
		key, rawValue, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !validCheckEnvKey(key) {
			return nil, checkEnvLineError(i+1, "expected KEY=value with a valid variable name")
		}
		value, err := parseCheckEnvValue(rawValue)
		if err != nil {
			return nil, checkEnvLineError(i+1, err.Error())
		}
		values[key] = value
	}
	return values, nil
}

func validCheckEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		c := key[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// Quoted values allow only a whitespace-separated trailing comment.
// Only escaped quotes and backslashes are unescaped inside double quotes.
func parseCheckEnvValue(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if trimmed[0] != '\'' && trimmed[0] != '"' {
		for i := range len(raw) {
			if raw[i] == '#' && i > 0 && checkEnvSpace(raw[i-1]) {
				return strings.TrimSpace(raw[:i]), nil
			}
			if raw[i] == '\'' || raw[i] == '"' {
				return "", errors.New("quotes must enclose the entire value")
			}
		}
		return trimmed, nil
	}
	quote := trimmed[0]
	var value strings.Builder
	for i := 1; i < len(trimmed); i++ {
		c := trimmed[i]
		if quote == '"' && c == '\\' && i+1 < len(trimmed) && (trimmed[i+1] == '\\' || trimmed[i+1] == '"') {
			i++
			value.WriteByte(trimmed[i])
			continue
		}
		if c == quote {
			tail := trimmed[i+1:]
			if tail == "" {
				return value.String(), nil
			}
			if checkEnvSpace(tail[0]) {
				tail = strings.TrimSpace(tail)
				if tail == "" || strings.HasPrefix(tail, "#") {
					return value.String(), nil
				}
			}
			return "", errors.New("unexpected text after quoted value")
		}
		value.WriteByte(c)
	}
	return "", errors.New("unterminated quoted value; multiline values are not supported")
}

func checkEnvSpace(c byte) bool { return c == ' ' || c == '\t' }

func checkEnvLineError(line int, detail string) error {
	// detail must always be authored text, never file content or an OS error.
	return fmt.Errorf("invalid env file at line %d: %s", line, detail)
}
