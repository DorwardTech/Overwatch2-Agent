// Package version carries the agent build version.
package version

// Value is the agent version. Override at build time with:
//
//	-ldflags "-X overwatch/agent/internal/version.Value=v1.2.3"
//
// or at runtime with the AGENT_VERSION environment variable.
var Value = "dev"
