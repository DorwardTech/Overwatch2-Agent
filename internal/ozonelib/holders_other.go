//go:build !unix && !windows

package ozonelib

func accessDenied(error) bool { return false }

func addrInUse(error) bool { return false }

func udpPortHolders(int) []string { return nil }
