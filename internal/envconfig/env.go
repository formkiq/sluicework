// Package envconfig reads explicitly selected, private environment files as data.
package envconfig

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

const maxBytes = 64 << 10

var assignment = regexp.MustCompile(`^(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)$`)

// ReadFile never executes shell expressions, expands variables or changes the
// process environment. The caller can validate the whole file before applying it.
func ReadFile(path string) (map[string]string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open environment file; select a private regular file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("environment file must be a private regular file (chmod 600)")
	}
	if info.Size() > maxBytes {
		return nil, errors.New("environment file exceeds 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return nil, errors.New("cannot read environment file within the 64 KiB limit")
	}
	return parse(string(data))
}

func parse(data string) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxBytes+1)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := assignment.FindStringSubmatch(text)
		if parts == nil {
			return nil, fmt.Errorf("environment file line %d: expected NAME=value", line)
		}
		if _, exists := values[parts[1]]; exists {
			return nil, fmt.Errorf("environment file line %d: duplicate variable", line)
		}
		value, err := decodeValue(parts[2])
		if err != nil {
			// Decoder errors can contain credentials; report only the line number.
			return nil, fmt.Errorf("environment file line %d: invalid single-line value", line)
		}
		values[parts[1]] = value
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot parse environment file")
	}
	return values, nil
}

func decodeValue(raw string) (string, error) {
	var value string
	if len(raw) > 0 && (raw[0] == '\'' || raw[0] == '"') {
		end, escaped := -1, false
		for i := 1; i < len(raw); i++ {
			if escaped {
				escaped = false
				continue
			}
			if raw[0] == '"' && raw[i] == '\\' {
				escaped = true
				continue
			}
			if raw[i] == raw[0] {
				end = i
				break
			}
		}
		if end < 0 {
			return "", errors.New("unterminated quote")
		}
		if rest := strings.TrimSpace(raw[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", errors.New("unexpected trailing text")
		}
		value = raw[1:end]
		if raw[0] == '"' {
			if err := json.Unmarshal([]byte(raw[:end+1]), &value); err != nil {
				return "", errors.New("invalid quoted value")
			}
		}
	} else {
		for i := 0; i < len(raw); i++ {
			if raw[i] == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
				raw = raw[:i]
				break
			}
		}
		value = strings.TrimSpace(raw)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("single-line values only")
	}
	return value, nil
}
