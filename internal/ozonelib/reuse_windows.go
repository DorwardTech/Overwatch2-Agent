//go:build windows

package ozonelib

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// setReuse sets SO_REUSEADDR so the port can be shared with other programs
// that also ask for reuse. A program that holds it exclusively still wins:
// the bind then fails, and the caller logs it and carries on.
func setReuse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}

// isMsgSize reports a datagram too large for the read buffer, which Windows
// reports as an error rather than truncating.
func isMsgSize(err error) bool { return errors.Is(err, windows.WSAEMSGSIZE) }
