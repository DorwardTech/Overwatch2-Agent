package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseEnv(t *testing.T) {
	in := "\uFEFF# Overwatch agent\r\n" +
		"\r\n" +
		"CENTRAL_API_URL=https://ow2.example/api/agent/ingest   # trailing comment\r\n" +
		"export AGENT_TOKEN='OW2_1_s#cret'\n" +
		"LOG_FILE=\"C:\\Program Files\\Overwatch Agent\\agent.log\"\n" +
		"  CACHE_DIR = C:\\ProgramData\\Overwatch Agent\\cache  \n" +
		"EMPTY=\n" +
		"HASH_INSIDE=a#b\n" +
		"QUOTED_HASH=\"keep # this\"\n"

	got, err := ParseEnv(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	want := []EnvVar{
		{"CENTRAL_API_URL", "https://ow2.example/api/agent/ingest"},
		{"AGENT_TOKEN", "OW2_1_s#cret"},
		{"LOG_FILE", `C:\Program Files\Overwatch Agent\agent.log`},
		{"CACHE_DIR", `C:\ProgramData\Overwatch Agent\cache`},
		{"EMPTY", ""},
		{"HASH_INSIDE", "a#b"},
		{"QUOTED_HASH", "keep # this"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseEnv =\n%v\nwant\n%v", got, want)
	}
}

func TestParseEnvRejectsMalformedLines(t *testing.T) {
	for _, in := range []string{"JUSTAWORD\n", "OK=1\n1BAD=x\n", "BAD KEY=x\n"} {
		_, err := ParseEnv(strings.NewReader(in))
		if err == nil {
			t.Errorf("ParseEnv(%q) accepted a malformed line", in)
			continue
		}
		if !strings.Contains(err.Error(), "line ") {
			t.Errorf("ParseEnv(%q) error %q does not say which line", in, err)
		}
	}
}

func TestLoadEnvFileEnvironmentWins(t *testing.T) {
	t.Setenv("OW_TEST_FROM_ENV", "environment")
	t.Setenv("OW_TEST_FROM_FILE", "")
	t.Setenv("OW_TEST_REPEATED", "")

	path := filepath.Join(t.TempDir(), "agent.env")
	content := "OW_TEST_FROM_ENV=file\nOW_TEST_FROM_FILE=file\nOW_TEST_REPEATED=first\nOW_TEST_REPEATED=second\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if n != 2 {
		t.Errorf("applied %d settings, want 2 (the environment-provided one is skipped)", n)
	}
	if got := os.Getenv("OW_TEST_FROM_ENV"); got != "environment" {
		t.Errorf("OW_TEST_FROM_ENV = %q; the file must not override the environment", got)
	}
	if got := os.Getenv("OW_TEST_FROM_FILE"); got != "file" {
		t.Errorf("OW_TEST_FROM_FILE = %q, want file", got)
	}
	if got := os.Getenv("OW_TEST_REPEATED"); got != "second" {
		t.Errorf("OW_TEST_REPEATED = %q; the last line in the file should win", got)
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	_, err := LoadEnvFile(filepath.Join(t.TempDir(), "absent.env"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist so an optional default file can be skipped", err)
	}
}
