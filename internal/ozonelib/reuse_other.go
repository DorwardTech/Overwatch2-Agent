//go:build !unix && !windows

package ozonelib

import "syscall"

func setReuse(_, _ string, _ syscall.RawConn) error { return nil }

func isMsgSize(error) bool { return false }
