//go:build unix

package ozonelib

import (
	"errors"
	"syscall"
)

// accessDenied is Windows' answer for a port held without sharing; here that
// is "address in use".
func accessDenied(error) bool { return false }

func addrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// udpPortHolders cannot name the holders here.
func udpPortHolders(int) []string { return nil }
