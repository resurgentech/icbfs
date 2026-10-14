package logd

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerActor names the account on the other end of a Unix socket, from
// the kernel's own record of it (SO_PEERCRED), so a client can't claim
// to be someone else.
func peerActor(conn net.Conn) string {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return "unknown"
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return "unknown"
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || cerr != nil {
		return "unknown"
	}
	return fmt.Sprintf("uid:%d", cred.Uid)
}
