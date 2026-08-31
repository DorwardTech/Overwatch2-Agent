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
