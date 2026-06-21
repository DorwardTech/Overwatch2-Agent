// Package ozonefix embeds the golden O-Zone Print Server fixtures and exposes
// them as a Go API. These payloads are the source of truth for the cache/proxy
// fidelity tests and back the fake O-Zone server (internal/ozonesim).
//
// Provenance and the wire-framing contract are documented in golden/README.md
// and docs/OZONE_PRINT_SERVER_API.md. Fixtures are valid JSON transcribed from
// the upstream spec's worked examples — never invented.
package ozonefix

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed golden/*.json golden/README.md
var goldenFS embed.FS

// Golden returns the raw bytes of a fixture file (e.g. "all_response.json").
// It panics if the fixture does not exist, since fixtures are compiled in.
func Golden(name string) []byte {
	b, err := goldenFS.ReadFile("golden/" + name)
	if err != nil {
		panic(fmt.Sprintf("ozonefix: missing golden %q: %v", name, err))
	}
	return b
}

// Named convenience accessors. Each returns the canonical JSON payload bytes.
func ListRequestJSON() []byte      { return Golden("list_request.json") }
func AllRequestJSON() []byte       { return Golden("all_request.json") }
func ListResponseJSON() []byte     { return Golden("list_response.json") }
func AllResponseJSON() []byte      { return Golden("all_response.json") }
func TextsBannerJSON() []byte      { return Golden("texts_banner.json") }
func EventTypesBannerJSON() []byte { return Golden("event_types_banner.json") }

// Names lists every golden JSON fixture (excludes the README).
func Names() []string {
	entries, err := goldenFS.ReadDir("golden")
	if err != nil {
		panic(fmt.Sprintf("ozonefix: cannot list golden dir: %v", err))
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if len(n) > 5 && n[len(n)-5:] == ".json" {
			names = append(names, n)
		}
	}
	return names
}

// Compact returns the JSON with insignificant whitespace removed. The O-Zone
// wire payload is compact JSON; tests frame Compact(golden) to compare against
// proxy output. It panics on invalid JSON (fixtures are validated in tests).
func Compact(jsonBytes []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, jsonBytes); err != nil {
		panic(fmt.Sprintf("ozonefix: invalid fixture JSON: %v", err))
	}
	return buf.Bytes()
}
