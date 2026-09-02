package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// EnvVar is one KEY=VALUE pair from a configuration file.
type EnvVar struct {
	Key, Value string
}

// LoadEnvFile reads a KEY=VALUE configuration file and applies every setting
// the process environment does not already provide. The environment wins, so
// what a container, a systemd unit or an operator's shell sets explicitly is
// never overridden by a file behind its back. It returns how many distinct
// settings were applied. A missing file is returned as the *os.PathError from
// opening it (wrapping fs.ErrNotExist), so a caller can treat an absent
// optional file as nothing to do.
//
// The agent has always been configured through the environment, which suits
// a container and little else: a Windows service has no environment of its
// own to speak of, and typing a dozen variables into the registry is not a
// procedure anyone should be handed. The file is the same lines any operator
// has already met in .env.example.
func LoadEnvFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	vars, err := ParseEnv(f)
	if err != nil {
		return 0, err
	}
	fromFile := map[string]bool{}
	for _, v := range vars {
		// The environment takes precedence; a later line in the same file
		// overrides an earlier one, as it would in a shell.
		if !fromFile[v.Key] && os.Getenv(v.Key) != "" {
			continue
		}
		if err := os.Setenv(v.Key, v.Value); err != nil {
			return len(fromFile), fmt.Errorf("set %s: %w", v.Key, err)
		}
		fromFile[v.Key] = true
	}
	return len(fromFile), nil
}

// ParseEnv parses the configuration file format: one KEY=VALUE per line.
// Blank lines and lines starting with # are ignored, a leading "export " is
// accepted, and a value wrapped in matching single or double quotes is taken
// literally (no escape processing — a Windows path needs no doubling of its
// backslashes). An unquoted value has any trailing " # comment" removed and
// surrounding whitespace trimmed. Windows line endings and a UTF-8 byte-order
// mark — Notepad writes one — are tolerated.
func ParseEnv(r io.Reader) ([]EnvVar, error) {
	var out []EnvVar
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		key = strings.TrimSpace(key)
		if !validEnvKey(key) {
			return nil, fmt.Errorf("line %d: invalid variable name %q", n, key)
		}
		out = append(out, EnvVar{Key: key, Value: parseEnvValue(strings.TrimSpace(val))})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseEnvValue(v string) string {
	if len(v) >= 2 {
		if q := v[0]; (q == '"' || q == '\'') && v[len(v)-1] == q {
			return v[1 : len(v)-1]
		}
	}
	// Unquoted: an inline comment begins at the first # preceded by whitespace,
	// so a # inside a value (a token, a URL fragment) is left alone.
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "\t#"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
