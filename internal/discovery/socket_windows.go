//go:build windows

package discovery

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

func listenBroadcastUDP() (*net.UDPConn, error) {
	var socketErr error
	config := net.ListenConfig{
		Control: func(_, _ string, raw syscall.RawConn) error {
			return raw.Control(func(fd uintptr) {
				socketErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_BROADCAST, 1)
			})
		},
	}
	connection, err := config.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	if socketErr != nil {
		_ = connection.Close()
		return nil, socketErr
	}
	return connection.(*net.UDPConn), nil
}
