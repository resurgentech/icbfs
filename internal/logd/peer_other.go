//go:build !linux

package logd

import "net"

// peerActor: only Linux says who's on the other end of the socket.
func peerActor(net.Conn) string { return "unknown" }
