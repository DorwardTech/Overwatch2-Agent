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

func TestMergeEnvKeepsEverythingItDoesNotManage(t *testing.T) {
	existing := `# Overwatch agent — venue 12
# Support note: the game server moved racks in June.

CENTRAL_API_URL=https://old.example/api/agent/ingest
AGENT_TOKEN=OW2_old

# ADMIN_API_ADDR=:8090      # the control panel, if we ever turn it on
NEXUS_DSN=user:pw@tcp(10.0.0.5:3306)/ng_system
`
	got := MergeEnv(existing, []EnvVar{
		{Key: "CENTRAL_API_URL", Value: "https://ow2.example/api/agent/ingest"},
		{Key: "AGENT_TOKEN", Value: "OW2_new"},
		{Key: "ADMIN_API_ADDR", Value: "0.0.0.0:8097"},
		{Key: "ENABLE_CACHE", Value: "true"},
	})

	want := `# Overwatch agent — venue 12
# Support note: the game server moved racks in June.

CENTRAL_API_URL=https://ow2.example/api/agent/ingest
AGENT_TOKEN=OW2_new

ADMIN_API_ADDR=0.0.0.0:8097
NEXUS_DSN=user:pw@tcp(10.0.0.5:3306)/ng_system

# --- Added by the setup page ---
ENABLE_CACHE=true
`
	if got != want {
		t.Errorf("MergeEnv =\n%q\nwant\n%q", got, want)
	}
}

func TestMergeEnvRoundTripsThroughTheParser(t *testing.T) {
	updates := []EnvVar{
		{Key: "AGENT_TOKEN", Value: "OW2_1_s#cret"},
		{Key: "CACHE_DIR", Value: `C:\ProgramData\Overwatch Agent\cache`},
		{Key: "SPACED", Value: " leading and trailing "},
	}
	merged := MergeEnv("", updates)

	parsed, err := ParseEnv(strings.NewReader(merged))
	if err != nil {
		t.Fatalf("the merged file does not parse: %v\n%s", err, merged)
	}
	got := map[string]string{}
	for _, v := range parsed {
		got[v.Key] = v.Value
	}
	for _, u := range updates {
		if got[u.Key] != u.Value {
			t.Errorf("%s round-tripped as %q, want %q", u.Key, got[u.Key], u.Value)
		}
	}
}

func TestMergeEnvEmptyValueCommentsOutAndCanBeRestored(t *testing.T) {
	// Unticking an option must not throw away what was there: the value stays
	// visible behind a comment marker, and ticking it again finds it.
	off := MergeEnv("ADMIN_API_ADDR=0.0.0.0:8097\n", []EnvVar{{Key: "ADMIN_API_ADDR", Value: ""}})
	if off != "# ADMIN_API_ADDR=0.0.0.0:8097\n" {
		t.Fatalf("turning the setting off gave %q", off)
	}
	on := MergeEnv(off, []EnvVar{{Key: "ADMIN_API_ADDR", Value: "0.0.0.0:9000"}})
	if on != "ADMIN_API_ADDR=0.0.0.0:9000\n" {
		t.Errorf("turning it back on gave %q", on)
	}
	vars, err := ParseEnv(strings.NewReader(off))
	if err != nil || len(vars) != 0 {
		t.Errorf("the commented-out file should define nothing; got %v (err %v)", vars, err)
	}
}

func TestMergeEnvDoesNotDuplicateOnRepeatedSaves(t *testing.T) {
	updates := []EnvVar{{Key: "OZONE_WS_HOST", Value: "10.0.0.5"}}
	once := MergeEnv("# a comment\n", updates)
	twice := MergeEnv(once, updates)
	if once != twice {
		t.Errorf("saving twice changed the file:\nfirst:\n%q\nsecond:\n%q", once, twice)
	}
	if strings.Count(twice, "OZONE_WS_HOST=") != 1 {
		t.Errorf("the setting appears more than once:\n%s", twice)
	}
}
