//go:build windows

package ozonelib

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetExtendedUdpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedUdpTable")

// udpTableOwnerPID asks GetExtendedUdpTable for each socket's owning process
// (UDP_TABLE_OWNER_PID).
const udpTableOwnerPID = 1

// maxHolders bounds the programs a message names.
const maxHolders = 3

func accessDenied(err error) bool { return errors.Is(err, windows.WSAEACCES) }

func addrInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }

// udpPortHolders names the processes bound to UDP port on the wildcard
// address, as "name.exe, PID n", or "PID n" when the name cannot be read (a
// process of another account the caller may not open). Only a wildcard socket
// can refuse the agent's own wildcard bind: one bound to a single address
// shares the port with it, so naming it would blame a program that is not in
// the way. Both tables are read, since a dual-stack socket bound to [::] holds
// the IPv4 port too. Best effort: nil when nothing can be read.
func udpPortHolders(port int) []string {
	seen := map[uint32]bool{}
	var out []string
	for _, af := range []uint32{windows.AF_INET, windows.AF_INET6} {
		for _, pid := range udpOwners(af, port) {
			if seen[pid] || len(out) == maxHolders {
				continue
			}
			seen[pid] = true
			out = append(out, processLabel(pid))
		}
	}
	return out
}

// udpOwners returns the owning PIDs of the af table's wildcard sockets on
// port, leaving out those whose owner is unknown (PID 0).
func udpOwners(af uint32, port int) []uint32 {
	if procGetExtendedUdpTable.Find() != nil {
		return nil
	}
	// MIB_UDPROW_OWNER_PID is local address, port, PID; the IPv6 row is a
	// 16-byte address and a scope id before the same two.
	rowSize, addrLen, portAt, pidAt := 12, 4, 4, 8
	if af == windows.AF_INET6 {
		rowSize, addrLen, portAt, pidAt = 28, 16, 20, 24
	}
	var size uint32
	var buf []byte
	// The table can grow between asking its size and reading it.
	for range 4 {
		var p unsafe.Pointer
		if len(buf) > 0 {
			p = unsafe.Pointer(&buf[0])
		}
		r, _, _ := procGetExtendedUdpTable.Call(uintptr(p), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), udpTableOwnerPID, 0)
		if r == 0 {
			break
		}
		if windows.Errno(r) != windows.ERROR_INSUFFICIENT_BUFFER {
			return nil
		}
		buf = make([]byte, size)
	}
	if len(buf) < 4 {
		return nil
	}
	var pids []uint32
	n := int(binary.LittleEndian.Uint32(buf))
	for i := range n {
		row := 4 + i*rowSize
		if row+rowSize > len(buf) {
			break
		}
		// The port is in network byte order, in the low two bytes.
		if int(buf[row+portAt])<<8|int(buf[row+portAt+1]) != port || !allZero(buf[row:row+addrLen]) {
			continue
		}
		if pid := binary.LittleEndian.Uint32(buf[row+pidAt:]); pid != 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// allZero reports an all-zero local address: the wildcard, 0.0.0.0 or [::].
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func processLabel(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Sprintf("PID %d", pid)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	name := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(h, 0, &name[0], &size); err != nil {
		return fmt.Sprintf("PID %d", pid)
	}
	return fmt.Sprintf("%s, PID %d", filepath.Base(windows.UTF16ToString(name[:size])), pid)
}
