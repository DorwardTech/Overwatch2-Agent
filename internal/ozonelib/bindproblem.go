package ozonelib

import (
	"fmt"
	"strings"
)

// portHolders names the programs bound to a UDP port, where this platform can
// say; replaced in tests.
var portHolders = udpPortHolders

// BindProblem says why the announcement port could not be opened, naming the
// programs that hold it when this platform can tell, or returns "" when the
// error says nothing more than itself. It gives the cause only: what to do
// instead (type the address, set OZONE_WS_HOST) depends on where it is shown.
//
// The agent opens the port for shared use, so a bind fails only when another
// program holds it without sharing it, or (on Windows) when the port lies in
// a range the system has reserved. Windows reports both as access denied
// (WSAEACCES) rather than "address in use".
func BindProblem(port int, err error) string {
	if err == nil {
		return ""
	}
	denied := accessDenied(err)
	if !denied && !addrInUse(err) {
		return ""
	}
	if names := portHolders(port); len(names) > 0 {
		return fmt.Sprintf("Another program on this computer (%s) holds UDP %d without sharing it.", strings.Join(names, "; "), port)
	}
	if denied {
		// Nobody found holding it: a reserved range is as likely.
		return fmt.Sprintf("Another program on this computer holds UDP %d without sharing it, or Windows has reserved the port "+
			"(`netsh interface ipv4 show excludedportrange protocol=udp` lists the reserved ranges).", port)
	}
	return fmt.Sprintf("Another program on this computer holds UDP %d without sharing it.", port)
}
