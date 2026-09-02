//go:build !windows

package platform

// defaultDataDir is empty: on Linux the container image (or the operator) sets
// every path explicitly, and the historical defaults stand.
func defaultDataDir() string { return "" }
