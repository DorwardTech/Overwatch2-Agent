//go:build unix

package ozonelib

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setReuse sets SO_REUSEADDR so the port can be shared. Not SO_REUSEPORT: on
// Linux that would split unicast traffic between the sockets sharing it.
func setReuse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}

// isMsgSize reports a datagram too large for the read buffer. Unix truncates
// it silently instead, which readLoop catches by its length.
func isMsgSize(error) bool { return false }
