package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

type bufferConn struct {
	*bytes.Reader
}

func (c *bufferConn) Write(data []byte) (int, error) { return len(data), nil }
func (c *bufferConn) Close() error                   { return nil }
func (c *bufferConn) LocalAddr() net.Addr            { return &net.TCPAddr{} }
func (c *bufferConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 9000}
}
func (c *bufferConn) SetDeadline(_ time.Time) error      { return nil }
func (c *bufferConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *bufferConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestProxyProtocolV1PreservesHTTPPayloadAndOriginalAddress(t *testing.T) {
	connection := &bufferConn{Reader: bytes.NewReader([]byte("PROXY TCP4 198.51.100.7 203.0.113.8 4567 443\r\nGET / HTTP/1.1\r\n"))}
	wrapped := &proxyProtocolConn{Conn: connection, reader: bufio.NewReader(connection), remote: connection.RemoteAddr()}
	matched, err := wrapped.readProxyHeader()
	if err != nil || !matched {
		t.Fatalf("read v1 header = %v, %v", matched, err)
	}
	if got := wrapped.RemoteAddr().String(); got != "198.51.100.7:4567" {
		t.Fatalf("remote address = %q", got)
	}
	payload, _ := io.ReadAll(wrapped)
	if string(payload) != "GET / HTTP/1.1\r\n" {
		t.Fatalf("remaining payload = %q", payload)
	}
}

func TestProxyProtocolV2ReadsOriginalIPv4Address(t *testing.T) {
	payload := make([]byte, 12)
	copy(payload[:4], net.ParseIP("192.0.2.9").To4())
	copy(payload[4:8], net.ParseIP("203.0.113.8").To4())
	binary.BigEndian.PutUint16(payload[8:10], 5050)
	binary.BigEndian.PutUint16(payload[10:12], 443)
	header := append(append([]byte(nil), proxyProtocolV2Signature...), 0x21, 0x11, 0, byte(len(payload)))
	stream := append(append(header, payload...), []byte("hello")...)
	connection := &bufferConn{Reader: bytes.NewReader(stream)}
	wrapped := &proxyProtocolConn{Conn: connection, reader: bufio.NewReader(connection), remote: connection.RemoteAddr()}
	matched, err := wrapped.readProxyHeader()
	if err != nil || !matched {
		t.Fatalf("read v2 header = %v, %v", matched, err)
	}
	if got := wrapped.RemoteAddr().String(); got != "192.0.2.9:5050" {
		t.Fatalf("remote address = %q", got)
	}
}
