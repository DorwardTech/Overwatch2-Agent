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

// MergeEnv applies updates to a configuration file's text, changing as little
// as possible. A setting already present is rewritten where it stands; one
// present only as a commented-out example is activated in place, keeping its
// position among the comments that explain it; anything else — the comments,
// the blank lines, the settings nothing here manages — is left exactly as it
// was. A setting given an empty value is commented out rather than deleted, so
// the value it had is still visible and turning it back on restores it.
//
// This matters because the file is edited by both a person and a program: the
// setup UI writes it, and the operator who opens it afterwards should find the
// file they were given, not a machine-generated one with their notes stripped.
func MergeEnv(existing string, updates []EnvVar) string {
	lines := strings.Split(existing, "\n")
	// A trailing newline leaves an empty final element; remember and restore it
	// so merging does not add or remove one.
	trailing := len(lines) > 0 && lines[len(lines)-1] == ""
	if trailing {
		lines = lines[:len(lines)-1]
	}

	var added []string
	for _, u := range updates {
		switch {
		case replaceLine(lines, u, activeSetting):
		case replaceLine(lines, u, commentedSetting):
		case u.Value != "":
			added = append(added, formatSetting(u))
		}
	}
	if len(added) > 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "# --- Added by the setup page ---")
		lines = append(lines, added...)
	}

	out := strings.Join(lines, "\n")
	if trailing || out == "" {
		out += "\n"
	}
	return out
}

// replaceLine rewrites the first line matching the given form of the key, and
// reports whether it found one.
func replaceLine(lines []string, u EnvVar, match func(line, key string) bool) bool {
	for i, line := range lines {
		if !match(line, u.Key) {
			continue
		}
		if u.Value == "" {
			// Keep the old value visible behind a comment marker: an operator
			// who turns the setting off and back on gets it back.
			lines[i] = "# " + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		} else {
			lines[i] = formatSetting(u)
		}
		return true
	}
	return false
}

// activeSetting reports whether the line sets key (not commented out).
func activeSetting(line, key string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") {
		return false
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, "export "))
	name, _, ok := strings.Cut(t, "=")
	return ok && strings.TrimSpace(name) == key
}

// commentedSetting reports whether the line is a commented-out example of key,
// as .env.example is full of ("# ADMIN_API_ADDR=:8090").
func commentedSetting(line, key string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return false
	}
	return activeSetting(strings.TrimSpace(strings.TrimLeft(t, "#")), key)
}

func formatSetting(u EnvVar) string { return u.Key + "=" + quoteEnvValue(u.Value) }

// quoteEnvValue quotes a value the parser would otherwise read differently:
// one carrying an inline-comment marker, or leading or trailing whitespace.
func quoteEnvValue(v string) string {
	needs := v != strings.TrimSpace(v) || strings.Contains(v, " #") || strings.Contains(v, "\t#")
	if !needs {
		return v
	}
	if strings.Contains(v, `"`) {
		return "'" + v + "'"
	}
	return `"` + v + `"`
}
