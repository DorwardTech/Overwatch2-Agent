package config

import (
	"strings"
	"testing"
)

// The legacy startup line used to print NEXUS_DSN verbatim, putting the Nexus
// password into container logs and anything that ships them. What survives is
// enough to tell one venue's database from another's, and nothing more.
func TestRedactDSN(t *testing.T) {
	cases := map[string]string{
		"reader:s3cr3t@tcp(10.0.0.9:3306)/ng_system?parseTime=true": "reader@tcp(10.0.0.9:3306)/ng_system",
		"reader@tcp(10.0.0.9:3306)/ng_system":                       "reader@tcp(10.0.0.9:3306)/ng_system",
		"reader:pa@ss:word@tcp(db:3306)/ng_system":                  "reader@tcp(db:3306)/ng_system",
		"":         "(not set)",
		"nonsense": "(set)",
	}
	for dsn, want := range cases {
		if got := RedactDSN(dsn); got != want {
			t.Errorf("RedactDSN(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// The point of the whole exercise: no password survives, whatever the shape.
func TestRedactDSNNeverLeaksThePassword(t *testing.T) {
	for _, dsn := range []string{
		"reader:hunter2@tcp(db:3306)/ng_system?parseTime=true",
		"reader:hunter2@tcp(db:3306)/ng_system",
		"reader:hunter2@unix(/var/run/mysqld/mysqld.sock)/ng_system",
		"root:hunter2@/ng_system",
	} {
		if got := RedactDSN(dsn); strings.Contains(got, "hunter2") {
			t.Errorf("RedactDSN(%q) = %q, which still carries the password", dsn, got)
		}
	}
}

// AGENT_MODE=Legacy in a hand-written file used to be read as an O-Zone venue:
// the comparison was case-sensitive, so the typo produced no error and the
// wrong half of the agent ran. The setup page reads the same file, so the two
// have to agree on what the word means.
func TestAgentModeIsCaseInsensitive(t *testing.T) {
	for _, word := range []string{"legacy", "Legacy", "LEGACY", " legacy "} {
		t.Setenv("CENTRAL_API_URL", "https://ow2.example/api/agent/ingest")
		t.Setenv("AGENT_TOKEN", "OW2_1_abc")
		t.Setenv("AGENT_MODE", word)
		t.Setenv("NEXUS_DSN", "u:p@tcp(h:3306)/ng_system")
		t.Setenv("LASERTAG_URL", "http://h/lasertag")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("AGENT_MODE=%q: %v", word, err)
		}
		if cfg.Mode != "legacy" {
			t.Errorf("AGENT_MODE=%q loaded as mode %q, want legacy", word, cfg.Mode)
		}
	}
}

// And the legacy settings stay unrequired for an O-Zone venue.
func TestOzoneModeDoesNotRequireTheLegacySettings(t *testing.T) {
	t.Setenv("CENTRAL_API_URL", "https://ow2.example/api/agent/ingest")
	t.Setenv("AGENT_TOKEN", "OW2_1_abc")
	t.Setenv("AGENT_MODE", "ozone")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("an O-Zone venue was rejected: %v", err)
	}
	if cfg.Mode != "ozone" {
		t.Errorf("mode = %q, want ozone", cfg.Mode)
	}
}

// The agent's own cache default stays off. Flipping it would turn the cache on
// for every venue in the fleet at the next upgrade, including the Linux ones
// that were never asked. The setup page defaults the checkbox on instead, which
// applies to a venue being configured and writes the choice explicitly — a
// different thing entirely.
func TestTheCacheDefaultsOffInTheAgent(t *testing.T) {
	t.Setenv("CENTRAL_API_URL", "https://ow2.example/api/agent/ingest")
	t.Setenv("AGENT_TOKEN", "OW2_1_abc")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheEnabled {
		t.Error("ENABLE_CACHE now defaults on — that changes every existing venue at upgrade")
	}
	if cfg.ProxyEnabled {
		t.Error("ENABLE_PROXY now defaults on")
	}
	if cfg.MsgBusEnabled {
		t.Error("ENABLE_MSG_BUS now defaults on")
	}
}

func centralEnv(t *testing.T) {
	t.Setenv("CENTRAL_API_URL", "https://ow2.example/api/agent/ingest")
	t.Setenv("AGENT_TOKEN", "OW2_1_abc")
}

// OZONE_WS_HOST=auto, in any case, turns discovery on and leaves no host to
// dial until one is chosen. An empty value still means this computer.
func TestAutoHostTurnsDiscoveryOn(t *testing.T) {
	for _, word := range []string{"auto", "AUTO", " Auto "} {
		centralEnv(t)
		t.Setenv("OZONE_WS_HOST", word)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%q: %v", word, err)
		}
		if !cfg.OzoneDiscover || cfg.OzoneHost != "" {
			t.Errorf("%q: discover=%v host=%q, want discovery and no host", word, cfg.OzoneDiscover, cfg.OzoneHost)
		}
	}

	centralEnv(t)
	t.Setenv("OZONE_WS_HOST", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OzoneDiscover || cfg.OzoneHost != "127.0.0.1" {
		t.Fatalf("empty host: discover=%v host=%q, want 127.0.0.1", cfg.OzoneDiscover, cfg.OzoneHost)
	}
	if cfg.LIB.Port != 12106 || !cfg.LIBListener {
		t.Fatalf("LIB = %+v, listener %v; want port 12106 and the report-only listener on by default", cfg.LIB, cfg.LIBListener)
	}
}

func TestAutoModeReadsTheSelectors(t *testing.T) {
	centralEnv(t)
	t.Setenv("OZONE_WS_HOST", "auto")
	t.Setenv("OZONE_SID", "0068")
	t.Setenv("OZONE_ARENA", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LIB.SID == nil || *cfg.LIB.SID != 68 || cfg.LIB.SIDRaw != "0068" || cfg.LIB.Arena == nil || *cfg.LIB.Arena != 1 {
		t.Fatalf("LIB = %+v", cfg.LIB)
	}
}

// In auto mode a malformed selector is an error: it would otherwise widen the
// choice to any server heard. With a fixed host the selectors mean nothing.
func TestBadSelectorsAreAnErrorOnlyInAutoMode(t *testing.T) {
	for key, value := range map[string]string{"OZONE_SID": "42a", "OZONE_ARENA": "256"} {
		centralEnv(t)
		t.Setenv(key, value)
		t.Setenv("OZONE_WS_HOST", "auto")
		if _, err := Load(); err == nil {
			t.Errorf("auto mode accepted %s=%s", key, value)
		}

		t.Setenv("OZONE_WS_HOST", "192.0.2.10")
		cfg, err := Load()
		if err != nil {
			t.Errorf("a fixed host was rejected over %s=%s: %v", key, value, err)
		}
		if cfg.LIB.SID != nil || cfg.LIB.Arena != nil {
			t.Errorf("a fixed host kept selectors: %+v", cfg.LIB)
		}
	}

	centralEnv(t)
	t.Setenv("OZONE_WS_HOST", "192.0.2.10")
	t.Setenv("OZONE_SID", "4242")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LIB.SID != nil || cfg.OzoneHost != "192.0.2.10" {
		t.Fatalf("fixed host: LIB = %+v, host %q", cfg.LIB, cfg.OzoneHost)
	}
}

func TestABadLIBPortIsAnError(t *testing.T) {
	centralEnv(t)
	t.Setenv("OZONE_LIB_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("OZONE_LIB_PORT=70000 was accepted")
	}
}

func TestLegacyModeIgnoresAuto(t *testing.T) {
	centralEnv(t)
	t.Setenv("AGENT_MODE", "legacy")
	t.Setenv("NEXUS_DSN", "u:p@tcp(h:3306)/ng_system")
	t.Setenv("LASERTAG_URL", "http://h/lasertag")
	t.Setenv("OZONE_WS_HOST", "auto")
	t.Setenv("OZONE_SID", "bad")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OzoneDiscover {
		t.Fatal("legacy mode turned discovery on")
	}
}
