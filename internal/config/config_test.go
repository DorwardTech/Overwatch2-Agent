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
